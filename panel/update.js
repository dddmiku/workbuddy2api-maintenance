/* ── 版本与热更新 ────────────────────────────────────
 * 状态来自网关的本机管理接口 GET /update；真正的升级动作走
 * POST /update/apply，由网关下载校验后把监听套接字交给新进程，
 * 旧进程继续把在途请求（含长流式对话）跑完再退出，所以前台不需要重连。
 *
 * 2026-10-13：点「立即更新」后弹出进度对话框，全程显示下载/校验/切换进度。
 */
var UP = { data:null, timer:null, loading:false, modal:false, modalBusy:false, modalDone:false };
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

// upDownBytes 与 upBytes 相同，但 0 显示为「0 B」而不是「—」。
//
// upBytes 的「—」语义是**缺失**（发布没给大小），而下载进度里的 0 是真实观测到的
// 「已下载 0 字节」。两者混用会让刚开始下载时显示「— / 19.1 MB」，看起来像字节数
// 没在动（2026-10-13 加进度条时发现）。
function upDownBytes(n){
  n = Number(n) || 0;
  return n === 0 ? '0 B' : upBytes(n);
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
  // 不可用有两种来源：配置关掉了（update.enabled=false），或发布仓库不开放
  // （私有且没配令牌）。两者都要如实说明原因，而不是让按钮点下去无声失败。
  var unavailable = (s.enabled === false) || !!s.unavailable_reason;
  if (unavailable){
    var why = s.enabled === false
      ? '热更新已在网关关闭（config update.enabled=false）'
      : (s.unavailable_reason || '发布仓库当前不可读');
    var mode = (s.visibility === 'private')
      ? '发布仓库为私有：把仓库改回公开，或配置 update.token'
      : '手工部署：本地构建运行包后上传替换，或让维护方协助';
    box.innerHTML = '<div class="upd-empty">' + esc(why) + '</div>' +
      mrow('当前版本', (s.current || '—') + (s.commit && s.commit !== 'unknown' ? ' · ' + s.commit.slice(0, 7) : '')) +
      mrow('发布仓库', (s.repo || '—') + (s.visibility ? ' · ' + s.visibility : '')) +
      mrow('升级方式', mode);
    $('#updState').textContent = s.enabled === false ? '已关闭' : '不可用';
    $('#updHint').textContent = s.enabled === false
      ? '如需恢复热更新，把 config 的 update.enabled 改回 true 并重载网关。'
      : (s.unavailable_reason || '');
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
  // 「立即更新」只在**确实有可升级版本**时可用：
  //   - 检查过且 update_ready=true → 可点；
  //   - 还没检查过（latest_tag 为空且无 checked_at）→ 可点，网关会先查一次远端；
  //   - 检查过但没有可升级版本（已是最新）→ 禁用，否则点下去会把服务白重启一遍。
  var checked = !!s.checked_at;
  var canApply = !busy && (!checked || !!s.update_ready);
  $('#btnUpdApply').disabled = !canApply;
  var hint;
  if (s.state === 'failed') hint = s.last_error || '更新失败';
  else if (busy) hint = '正在' + (UP_STATE[s.state].t) + '…升级期间现有对话不会中断';
  else if (s.update_ready) hint = '可升级到 ' + s.latest_tag;
  else if (s.checked_at) hint = '已是最新版本（' + (s.latest_tag || s.current || '') + '）';
  else hint = '点「立即更新」会先检查远端版本';
  $('#updHint').textContent = hint;
  renderUpdateModal(s);
}

// ── 更新进度对话框 ──────────────────────────────────────────────────────
//
// 点「立即更新」后弹出，直到成功或失败才自动收起。它读的是与卡片**同一个**
// /update 快照（renderUpdate 每次都会调过来），所以进度与卡片永远一致。

/** 打开进度对话框。 */
function openUpdateModal(){
  UP.modal = true;
  UP.modalBusy = false;
  UP.modalDone = false;
  $('#updModalTitle').textContent = '正在更新';
  // 图标类必须**显式重置**：上一次失败时它被设成 'mic e'，不重置的话重试时
  // 标题写着「正在更新」而图标还是红色叹号，自相矛盾。
  $('#updModalIcon').className = 'mic w';
  $('#updModalText').textContent = '正在准备…';
  $('#updModalNote').textContent = '';
  var bar = $('#updModalBar');
  bar.classList.remove('unknown');
  bar.querySelector('i').style.width = '0%';
  $('#updModalClose').textContent = '后台运行';
  $('#updModalClose').disabled = false;
  $('#scrimU').classList.add('on');
  $('#updModal').classList.add('on');
}

function closeUpdateModal(){
  UP.modal = false;
  $('#scrimU').classList.remove('on');
  $('#updModal').classList.remove('on');
}

/** 按当前状态刷新对话框。没打开时什么都不做。 */
function renderUpdateModal(s){
  if (!UP.modal) return;
  var bar = $('#updModalBar');
  var fill = bar.querySelector('i');
  var text = $('#updModalText');
  var note = $('#updModalNote');
  var state = s.state || 'idle';
  var total = Number(s.asset_size) || 0;
  var done = Number(s.downloaded_bytes) || 0;

  if (state === 'failed'){
    // 终态：把原因写清楚，按钮变成「关闭」。不自动收起——用户需要读到失败原因。
    UP.modalDone = true;
    $('#updModalTitle').textContent = '更新失败';
    $('#updModalIcon').className = 'mic e';
    text.textContent = s.last_error || '更新失败，请查看网关日志。';
    bar.classList.remove('unknown');
    fill.style.width = '0%';
    note.textContent = '';
    $('#updModalClose').textContent = '关闭';
    return;
  }

  if (state === 'checking' || state === 'downloading' || state === 'handover'){
    UP.modalBusy = true;
  }

  if (state === 'checking'){
    text.textContent = '正在检查远端版本…';
    bar.classList.add('unknown');
    note.textContent = '';
    return;
  }

  if (state === 'downloading'){
    // 总大小未知（发布没给 size）时用不确定态滑动，而不是显示一个假的 0%
    // ——那会让人以为卡住了。
    //
    // 已下载字节一律用 upDownBytes 而不是 upBytes：upBytes(0) 是「—」（它的语义
    // 是「缺失」），但这里的 0 是真实的「还没下到字节」，显示成「— / 19.1 MB」
    // 会让人以为字节数根本没在动。
    if (total > 0){
      var pct = Math.max(0, Math.min(100, Math.round((done / total) * 100)));
      bar.classList.remove('unknown');
      fill.style.width = pct + '%';
      text.textContent = '正在下载更新包…';
      note.innerHTML = '<b>' + upDownBytes(done) + '</b> / ' + upBytes(total) + ' · ' + pct + '%';
    } else {
      bar.classList.add('unknown');
      text.textContent = '正在下载更新包…';
      note.innerHTML = '<b>' + upDownBytes(done) + '</b>';
    }
    return;
  }

  if (state === 'handover'){
    // 下载已完成：进度条补满，明确告诉用户「不会再等很久」。
    bar.classList.remove('unknown');
    fill.style.width = '100%';
    text.textContent = '下载完成，正在切换到新版本…';
    note.textContent = '正在进行的对话会继续跑完，不会中断。';
    return;
  }

  // 走到 idle 时**不能**直接宣布成功：申请提交后的第一次轮询可能比网关的状态
  // 更新更早，此时 state 还是 idle，会把「刚开始」误报成「已完成」。
  // 只有确实经历过 checking/downloading/handover 之后回到 idle，才算完成。
  if (state === 'idle' && UP.modalBusy){
    UP.modalDone = true;
    bar.classList.remove('unknown');
    fill.style.width = '100%';
    $('#updModalTitle').textContent = '更新完成';
    $('#updModalIcon').className = 'mic ok';
    text.textContent = '已切换到新版本 ' + (s.current || '') + '。';
    note.textContent = '页面稍后会自动恢复；若长时间无响应，手动刷新一次即可。';
    $('#updModalClose').textContent = '关闭';
    return;
  }

  // 还没看到任何进行中的状态：保持「正在准备…」，等下一轮轮询。
  text.textContent = '正在准备…';
  bar.classList.add('unknown');
  note.textContent = '';
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
    var s = (UP.data && UP.data.status) || {};
    // 交接完成后旧进程随时会退出，继续轮询会看到连接错误，到点就停。
    if (ticks > 240 || s.state === 'failed') stopUpdatePoll();
    // 走到 idle 且对话框已经看过下载/切换阶段 → 更新完成，停轮询。
    if (UP.modalDone && s.state === 'idle') stopUpdatePoll();  }, 2000);
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
  // 兜底：按钮已按 update_ready 禁用，这里再挡一次，避免通过其它入口触发无意义的重启。
  if (s.checked_at && !s.update_ready){
    toast('已是最新版本（' + (s.latest_tag || s.current || '') + '），无需更新', 'ok');
    return;
  }
  ask('立即更新',
    '将下载并切换到 ' + (s.latest_tag || '最新版本') + '。升级时正在进行的对话会继续跑完，不会中断；' +
    '控制台页面可能会短暂失去响应，稍后自动恢复。',
    '开始更新', false, async function(){
      try{
        var r = await api('api/update/apply', {});
        if (r.ok === false){ toast(r.message || '更新未开始', 'err'); await loadUpdate(); return; }
        // 点完确认就直接弹进度框——用户此刻最想知道的就是「在下没在动」。
        // 先弹再拉数据，避免开头几秒没有任何反馈。
        openUpdateModal();
        renderUpdate();
        startUpdatePoll();
      }catch(err){
        toast(err.message, 'err');
      }
    });
}

document.addEventListener('click', function(e){
  if (e.target.closest('#btnUpdCheck')){ actUpdateCheck(); return; }
  if (e.target.closest('#btnUpdApply')){ actUpdateApply(); return; }
  // 「后台运行」只收起对话框，**不取消更新**：更新跑在网关进程里，关掉界面
  // 不会影响它；用户想走开时不该被一个对话框按住。
  if (e.target.closest('#updModalClose')){ closeUpdateModal(); return; }
});
$('#scrimU').addEventListener('click', closeUpdateModal);

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
