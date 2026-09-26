/* ── 版本与热更新 ────────────────────────────────────
 * 状态来自网关的本机管理接口 GET /update；真正的升级动作走
 * POST /update/apply，由网关下载校验后把监听套接字交给新进程，
 * 旧进程继续把在途请求（含长流式对话）跑完再退出，所以前台不需要重连。
 */
var UP = { data:null, timer:null, loading:false };
var UP_STATE = {
  idle:      { t:'空闲',   cls:''   },
  checking:  { t:'检查中', cls:'w'  },
  downloading:{ t:'下载中', cls:'w' },
  handover:  { t:'切换中', cls:'w'  },
  failed:    { t:'失败',   cls:'e'  }
};
var UP_BUSY = { checking:1, downloading:1, handover:1 };

function upState(st){
  var m = UP_STATE[st] || { t: st || '未知', cls:'' };
  return '<span class="upd-tag ' + m.cls + '">' + esc(m.t) + '</span>';
}

function upBytes(n){
  if (!n) return '—';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(0) + ' KB';
  return (n / 1048576).toFixed(1) + ' MB';
}

function renderUpdate(){
  var r = UP.data, box = $('#updRows');
  if (!box) return;
  if (!r || r.ok === false){
    box.innerHTML = '<div class="upd-empty">' + esc((r && r.message) || '网关未响应热更新接口（旧版本网关请先升级）') + '</div>';
    $('#updState').textContent = '不可用';
    $('#updHint').textContent = '';
    $('#btnUpdApply').disabled = true;
    $('#btnUpdCheck').disabled = true;
    return;
  }
  var s = r.status || {};
  if (s.enabled === false){
    // 网关关掉了热更新（config update.enabled=false）：如实显示，按钮禁用，
    // 不再让「立即更新」点下去只弹一句"已开始"却什么也不做。
    box.innerHTML = '<div class="upd-empty">热更新已在网关关闭（config update.enabled=false）</div>' +
      mrow('当前版本', (s.current || '—') + (s.commit && s.commit !== 'unknown' ? ' · ' + s.commit.slice(0, 7) : '')) +
      mrow('升级方式', '手工部署：本地构建运行包后上传替换，或让维护方协助');
    $('#updState').textContent = '已关闭';
    $('#updHint').textContent = '如需恢复热更新，把 config 的 update.enabled 改回 true 并重载网关。';
    $('#btnUpdApply').disabled = true;
    $('#btnUpdCheck').disabled = true;
    return;
  }
  var busy = !!UP_BUSY[s.state];
  var current = s.current || '—';
  var commit = (s.commit && s.commit !== 'unknown') ? ' · ' + s.commit.slice(0, 7) : '';
  var latest = s.latest_tag || (s.checked_at ? '已是最新' : '未检查');
  var rows =
    mrow('当前版本', current + commit) +
    mrow('构建时间', s.built_at && s.built_at !== 'unknown' ? fmtTime(s.built_at) : '—') +
    mrow('远端最新', latest, s.update_ready ? 'w' : 'ok') +
    mrow('更新状态', UP_STATE[s.state] ? UP_STATE[s.state].t : (s.state || '—'),
      (UP_STATE[s.state] || {}).cls || '') +
    mrow('下载包', s.asset_name ? (s.asset_name + ' · ' + upBytes(s.asset_size)) : '—') +
    mrow('上次检查', s.checked_at ? relTime(s.checked_at) : '—') +
    mrow('仓库', s.repo || '—');
  box.innerHTML = rows;
  $('#updState').innerHTML = upState(s.state);
  $('#btnUpdCheck').disabled = busy;
  // 未检查过远端时也允许点「立即更新」：网关自己会先查一次远端版本。
  $('#btnUpdApply').disabled = busy;
  var hint;
  if (s.state === 'failed') hint = s.last_error || '更新失败';
  else if (busy) hint = '正在' + (UP_STATE[s.state].t) + '…升级期间现有对话不会中断';
  else if (s.update_ready) hint = '可升级到 ' + s.latest_tag;
  else if (s.checked_at) hint = '已是最新版本';
  else hint = '点「立即更新」会先检查远端版本';
  $('#updHint').textContent = hint;
}

async function loadUpdate(){
  if (UP.loading) return;
  UP.loading = true;
  try{
    UP.data = await api('api/update');
  }catch(err){
    UP.data = { ok:false, message: err.message };
  }finally{
    UP.loading = false;
  }
  renderUpdate();
}

function stopUpdatePoll(){
  if (UP.timer){ clearInterval(UP.timer); UP.timer = null; }
}

function startUpdatePoll(){
  stopUpdatePoll();
  var ticks = 0;
  UP.timer = setInterval(async function(){
    ticks++;
    await loadUpdate();
    var st = UP.data && UP.data.status && UP.data.status.state;
    // 交接完成后旧进程随时会退出，继续轮询会看到连接错误，到点就停。
    if (ticks > 240 || st === 'failed') stopUpdatePoll();
  }, 2000);
}

async function actUpdateCheck(){
  $('#btnUpdCheck').disabled = true;
  try{
    var r = await api('api/update/check', {});
    UP.data = r.ok === false ? r : r;
    renderUpdate();
    toast(r.ok === false ? ((r.message) || '检查失败') : '已检查远端版本', r.ok === false ? 'err' : 'ok');
  }catch(err){
    toast(err.message, 'err');
    $('#btnUpdCheck').disabled = false;
  }
}

async function actUpdateApply(){
  var s = (UP.data && UP.data.status) || {};
  ask('立即更新',
    '将下载并切换到 ' + (s.latest_tag || '最新版本') + '。升级时正在进行的对话会继续跑完，不会中断；' +
    '控制台页面可能会短暂失去响应，稍后自动恢复。',
    '开始更新', false, async function(){
      try{
        var r = await api('api/update/apply', {});
        if (r.ok === false){ toast(r.message || '更新未开始', 'err'); await loadUpdate(); return; }
        toast('已开始更新，正在下载与切换', 'ok');
        renderUpdate();
        startUpdatePoll();
      }catch(err){
        toast(err.message, 'err');
      }
    });
}

document.addEventListener('click', function(e){
  if (e.target.closest('#btnUpdCheck')){ actUpdateCheck(); return; }
  if (e.target.closest('#btnUpdApply')){ actUpdateApply(); }
});

/* ── 重复推理保护（命中后重发 / 命中即停止）────────────────
 * 运行期开关，走网关本机管理接口 GET/POST /features/reasoning-loop。
 * 与 config.json 的 features.reasoning_loop_stop_only 同义，但改完立即生效。
 */
var RL = { data:null, loading:false };

function renderReasoningLoop(){
  var box = $('#rlRows');
  if (!box) return;
  var r = RL.data;
  if (!r || r.ok === false){
    box.innerHTML = '<div class="upd-empty">' +
      esc((r && r.message) || '网关未响应（旧版本网关请先升级）') + '</div>';
    $('#rlState').textContent = '不可用';
    $('#rlStopOnly').disabled = true;
    return;
  }
  var stopOnly = !!r.stop_only;
  $('#rlStopOnly').checked = stopOnly;
  $('#rlStopOnly').disabled = false;
  $('#rlState').innerHTML = '<span class="upd-tag ' + (stopOnly ? 'w' : '') + '">' +
    (stopOnly ? '命中即停止' : '命中后自动重发') + '</span>';
  box.innerHTML =
    mrow('当前行为', stopOnly ? '命中即停止，不重发' : '命中后同账号重发一次', stopOnly ? 'w' : 'ok') +
    mrow('启动配置', r.default ? '只停不重发' : '命中后重发') +
    mrow('生效方式', '立即生效，不需要重启');
}

async function loadReasoningLoop(){
  if (RL.loading) return;
  RL.loading = true;
  try{
    RL.data = await api('api/features/reasoning-loop');
  }catch(err){
    RL.data = { ok:false, message: err.message };
  }finally{
    RL.loading = false;
  }
  renderReasoningLoop();
}

async function actReasoningLoopToggle(next){
  var input = $('#rlStopOnly');
  input.disabled = true;
  try{
    var r = await api('api/features/reasoning-loop', { stop_only: next });
    if (r.ok === false){ toast(r.message || '切换失败', 'err'); }
    else { toast(r.message || '已切换', 'ok'); }
    await loadReasoningLoop();
  }catch(err){
    toast('切换失败：' + (err && err.message || err), 'err');
    // 失败时把开关恢复成服务端当前值，避免界面显示与实际不一致。
    await loadReasoningLoop();
  }finally{
    input.disabled = false;
  }
}

document.addEventListener('change', function(e){
  if (e.target && e.target.id === 'rlStopOnly') actReasoningLoopToggle(e.target.checked);
});
