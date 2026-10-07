"use strict";
// ═══ 更新日志 ═══
// 2026-09-25：接入请求消费明细导航与刷新，保持筛选和历史分页状态。
// 2026-09-24：忽略过时刷新响应，日志按完整行检测更新并补取改变后的条数；修正空成功时间、凭证到期判断和当前页刷新。
// 2026-09-24：冷却截止时间随数据快照固定，搜索或切页不再重新开始倒计时；同一到期快照仅补取一次。
// 2026-09-22：系统页新增「重复推理保护」热切换开关（命中后重发 / 命中即停止），
//             进入页面即拉取当前运行期值，改完立即生效、不需要重启网关。
// 2026-09-20：按 new-api 面板规范重做外观：顶栏横跨整宽并承载品牌与全局操作，
//             页标题移入内容区，主题按钮从侧栏底部移到顶栏（不再需要 themeLabel）。
// 2026-09-20：渲染按当前页收敛——刷新只重绘可见页（此前每次 30 秒刷新都重建全部
//             页面的 DOM，长表格页在刷新瞬间会明显卡顿）；搜索输入加防抖。
// 2026-09-20：2.1.2 冷却状态带实时倒计时（data-cool-end + 每秒就地刷新，归零自动补取
//             一次数据），并把分钟级冷却显示成 mm:ss。
// 2026-09-20：2.1.2 账号表状态列改显示冷却剩余与原因，最近活动区分「无成功记录」与
//             「无记录」——此前冷却中的号把凭证有效期（剩 363 天）显示在「冷却中」旁，
//             且只有错误记录时整列显示「从未」，两处都误导排查。
// 2026-09-20：侧栏显示管理台实际版本，避免发布 2.1.1 后仍标为 v1。
// 2026-09-19：积分查询失败时显示错误和缓存状态，避免把尚未取到的余额显示为零。
// 2026-09-18：所有管理写请求携带同源标记，覆盖账号、任务、会话和服务操作。
// 2026-09-18：退出失败时保留页面并显示错误，不再跳转伪装成功；重启提示与后台收尾行为保持一致。
// 2026-09-16：接入密钥管理导航与本页刷新，侧栏不再下发或复制完整配置密钥。

var $  = function(s, r){ return (r || document).querySelector(s); };
var $$ = function(s, r){ return Array.prototype.slice.call((r || document).querySelectorAll(s)); };
function esc(t){ return String(t == null ? '' : t).replace(/[&<>"']/g, function(c){
  return ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'})[c]; }); }
function pad(n){ return n < 10 ? '0' + n : '' + n; }
function num(n){ return (n == null || isNaN(n)) ? '—' : Number(n).toLocaleString('en-US'); }
// compactNum：日志表里的输入/缓存列。非数字（"-" 等缺失标记）原样返回，
// 数字按 k/M 缩写，鼠标悬浮看原值（title 由调用方给出）。
function compactNum(v){
  if (v == null || v === '') return '-';
  var n = Number(String(v).replace(/[, ]/g, ''));
  if (isNaN(n)) return String(v);
  if (n >= 1000000000) return (n / 1000000000).toFixed(2) + 'B';
  if (n >= 1000000) return (n / 1000000).toFixed(2) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'k';
  return String(n);
}
// debounce 让高频输入（搜索框）只在停顿后触发一次渲染。
function debounce(fn, wait){
  var t = null;
  return function(){
    var self = this, args = arguments;
    if (t) clearTimeout(t);
    t = setTimeout(function(){ t = null; fn.apply(self, args); }, wait);
  };
}
function shortUid(u){ return (u && u.length > 18) ? u.slice(0, 8) + '…' + u.slice(-6) : (u || '—'); }
function fmtTime(iso){
  if (!iso) return '—';
  var s = String(iso);
  if (s.indexOf('0001-01-01') === 0) return '—';
  var d = new Date(s);
  if (isNaN(d.getTime())) return '—';
  return pad(d.getMonth()+1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
}
function relTime(iso){
  if (!iso) return '从未';
  var s = String(iso);
  if (s.indexOf('0001-01-01') === 0) return '从未';
  var d = new Date(s);
  if (isNaN(d.getTime())) return '从未';
  var diff = Math.floor((Date.now() - d.getTime()) / 1000);
  if (diff < 0) return '即将';
  if (diff < 60) return '刚刚';
  if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
  if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
  if (diff < 2592000) return Math.floor(diff / 86400) + ' 天前';
  return fmtTime(iso);
}
function daysLeft(sec){
  if (!sec || !isFinite(Number(sec))) return null;
  return Math.ceil((Number(sec) * 1000 - Date.now()) / 86400000);
}
function activityTime(iso){
  if (!iso) return '';
  var s = String(iso);
  return s.indexOf('0001-01-01') === 0 || isNaN(new Date(s).getTime()) ? '' : s;
}
// coolLeft 把冷却剩余秒数格式化成人读文案。冷却与凭证有效期是两回事，状态列在冷却中
// 必须显示冷却剩余，否则「冷却中 + 剩 363 天」会被读成冷却要等一年（用户实测反馈）。
function coolLeft(sec){
  var s = Number(sec) || 0;
  if (s <= 0) return '即将恢复';
  if (s < 60) return '剩 ' + s + ' 秒';
  if (s < 3600) {
    var mm = Math.floor(s / 60), ss = s % 60;
    return '剩 ' + mm + ':' + (ss < 10 ? '0' : '') + ss;
  }
  if (s < 86400) {
    var h = Math.floor(s / 3600), m = Math.round((s % 3600) / 60);
    return '剩 ' + h + ' 小时' + (m ? ' ' + m + ' 分' : '');
  }
  return '剩 ' + Math.round(s / 86400) + ' 天';
}

// 冷却倒计时：状态列只在下一次拉取数据时才重算，用户盯着页面看不到秒数变化。
// 渲染时把截止时刻写进 data-cool-end，这里每秒就地更新文本；有账号归零时补一次
// 刷新，让「冷却中」及时翻成可用状态。
var COOL_TICK = null, COOL_RELOAD_PENDING = false;
function startCoolTicker(){
  if (COOL_TICK) return;
  COOL_TICK = setInterval(tickCooldowns, 1000);
}
function tickCooldowns(){
  var nodes = document.querySelectorAll('[data-cool-end]');
  if (!nodes.length) return;
  var now = Date.now(), expired = [];
  for (var i = 0; i < nodes.length; i++){
    var node = nodes[i];
    var end = Number(node.getAttribute('data-cool-end')) || 0;
    var left = Math.max(0, Math.round((end - now) / 1000));
    var reason = node.getAttribute('data-cool-reason') || '';
    if (left <= 0 && node.getAttribute('data-cool-refreshed') !== '1') expired.push(node);
    node.textContent = coolLeft(left) + (reason ? ' · ' + reason : '');
  }
  if (!expired.length || COOL_RELOAD_PENDING) return;
  // 归零后只补取一次：等后端把这号从冷却里摘掉再渲染，避免每秒重拉。
  // 页面在后台时不补取——用户看不到，等切回来时 30s 定时器自然会拉新数据。
  if (document.hidden) return;
  expired.forEach(function(node){ node.setAttribute('data-cool-refreshed', '1'); });
  COOL_RELOAD_PENDING = true;
  setTimeout(async function(){
    try { await loadAll(); } finally { COOL_RELOAD_PENDING = false; }
  }, 1500);
}
// coolReason 冷却原因转可读文案。上游原始 reason 已是运维可读串（如 "429 rate limit"），
// 只做英文短语到中文的映射，未知值原样透出，不编造。
function coolReason(p){
  var r = String((p && p.reason) || '');
  if (!r) return '';
  if (r.indexOf('429') >= 0) return '上游限流';
  if (r.indexOf('14017') >= 0) return 'trial 未激活';
  if (r.indexOf('6004') >= 0) return '模型限流';
  if (r.indexOf('11140') >= 0) return '账号被上游封禁';
  if (r.indexOf('404') >= 0) return '上游 404';
  if (r.indexOf('余额') >= 0) return '余额不足';
  return r;
}
function svg(inner, size){
  return '<svg width="' + (size||16) + '" height="' + (size||16) + '" viewBox="0 0 24 24" fill="none" ' +
    'stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">' + inner + '</svg>';
}
var IC = {
  users: '<path d="M15 20.5v-1.6a3.4 3.4 0 0 0-3.4-3.4H6.4A3.4 3.4 0 0 0 3 18.9v1.6"/><circle cx="9" cy="7.6" r="3.6"/><path d="M21 20.5v-1.6a3.4 3.4 0 0 0-2.6-3.3"/>',
  check: '<circle cx="12" cy="12" r="8.6"/><path d="m8.4 12.2 2.5 2.5 4.7-5.2"/>',
  clock: '<circle cx="12" cy="12" r="8.6"/><path d="M12 7.4V12l3 1.8"/>',
  warn:  '<path d="M12 9.5v4M12 17h.01"/><path d="M10.3 4.4 2.9 17.2A1.9 1.9 0 0 0 4.6 20h14.8a1.9 1.9 0 0 0 1.7-2.8L13.7 4.4a1.9 1.9 0 0 0-3.4 0Z"/>',
  box:   '<path d="M20.5 7.5 12 3 3.5 7.5v9L12 21l8.5-4.5v-9Z"/><path d="m3.5 7.5 8.5 4.5 8.5-4.5"/>',
  coins: '<circle cx="12" cy="12" r="8.6"/><path d="M12 7.4v9.2M9.4 10h5.2M9.4 14h5.2"/>',
  eye:   '<path d="M2.2 12S5.8 5.4 12 5.4 21.8 12 21.8 12 18.2 18.6 12 18.6 2.2 12 2.2 12Z"/><circle cx="12" cy="12" r="2.7"/>',
  play:  '<path d="M8 5.4v13.2L19 12 8 5.4Z"/>',
  trash: '<path d="M4 7h16M9.6 7V4.8h4.8V7M6.6 7l.9 13.2h9l.9-13.2"/>',
  pulse: '<path d="M3 12h3.6l2.4-6 3.9 12 2.6-6H21"/>',
  moon:  '<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z"/>',
  sun:   '<circle cx="12" cy="12" r="4"/><path d="M12 2.6v2.2M12 19.2v2.2M2.6 12h2.2M19.2 12h2.2M5.3 5.3l1.6 1.6M17.1 17.1l1.6 1.6M18.7 5.3l-1.6 1.6M6.9 17.1l-1.6 1.6"/>'
};

/* ── 状态 ─────────────────────────────────────────── */
var S = { data:null, tasks:null, filter:'all', q:'', logLines:120, loadRevision:0, loadedAt:0 };
var DT = { realm:'cn', url:'', loginId:'', timer:null, tries:0, polling:false };
var modalCb = null;
var logKey = null, logName = '';

/* ── 主题 ─────────────────────────────────────────── */
function setTheme(t){
  document.documentElement.setAttribute('data-theme', t);
  $('#icTheme').innerHTML = (t === 'dark') ? IC.moon : IC.sun;
  $('#btnTheme').setAttribute('title', (t === 'dark') ? '切换到亮色' : '切换到暗色');
  try { localStorage.setItem('wb2a-theme', t); } catch(e){}
}
$('#btnTheme').addEventListener('click', function(){
  setTheme(document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark');
});
(function(){
  var t = 'dark';
  try { var v = localStorage.getItem('wb2a-theme'); if (v) t = v; } catch(e){}
  setTheme(t);
})();

/* ── 通讯 ─────────────────────────────────────────── */
async function api(path, body){
  var opt = { headers:{ 'Content-Type':'application/json' }, cache:'no-store' };
  // 所有写操作统一携带同源管理标记。
  if (body !== undefined){
    opt.headers['X-Admin-Request'] = '1';
  }
  if (body !== undefined){ opt.method = 'POST'; opt.body = JSON.stringify(body || {}); }
  var r = await fetch(path, opt);
  if (r.status === 401){ location.replace('login'); throw new Error('登录已失效'); }
  var data = null;
  try{ data = await r.json(); }catch(e){ data = null; }
  if (!r.ok){
    var msg = (data && (data.message || (data.error && data.error.message))) ||
      ('HTTP ' + r.status);
    var err = new Error(msg);
    err.status = r.status;
    throw err;
  }
  return data;
}
function toast(msg, kind){
  var d = document.createElement('div');
  d.className = 'toast ' + (kind || '');
  var icon = kind === 'ok' ? IC.check : (kind === 'err' ? IC.warn : IC.clock);
  d.innerHTML = svg(icon, 15) + '<div>' + esc(msg) + '</div>';
  $('#toasts').appendChild(d);
  setTimeout(function(){ d.style.transition = 'opacity .25s'; d.style.opacity = '0';
    setTimeout(function(){ d.remove(); }, 260); }, 4200);
}

/* ── 视图 ─────────────────────────────────────────── */
var PAGE = {
  overview:{ t:'概览', d:'账号池与运行状态' },
  accounts:{ t:'账号', d:'凭证、积分与启停' },
  keys:    { t:'密钥管理', d:'创建与管理客户端的访问密钥' },
  usage:   { t:'用量统计', d:'按 API key 累计的 token 用量' },
  requests:{ t:'请求明细', d:'核对每次调用、重试消费与账号调度' },
  tasks:   { t:'排程', d:'定时任务开关与手动触发' },
  logs:    { t:'请求日志', d:'每次请求一行，含调用密钥、账号与耗时' },
  system:  { t:'系统', d:'服务状态、登录账号与运行日志' }
};
var sysLoaded = false;
// CURRENT_VIEW 当前页标识：刷新时只重绘这一页（见 render）。
var CURRENT_VIEW = 'overview';
function go(v){
  if (!PAGE[v]) v = 'overview';
  CURRENT_VIEW = v;
  $$('.view[data-view]').forEach(function(p){
    p.classList.toggle('hide', p.getAttribute('data-view') !== v);
  });
  $$('#tabs button, #tabs2 button').forEach(function(b){
    b.classList.toggle('on', b.getAttribute('data-view') === v);
  });
  $('#pgTitle').textContent = PAGE[v].t;
  $('#pgDesc').textContent = PAGE[v].d;
  document.title = PAGE[v].t + ' · workbuddy2api';
  try { localStorage.setItem('wb2a-view', v); } catch(e){}
  history.replaceState(null, '', '#' + v);
  closeNav();
  // 进入页面立即用已有数据渲染一次：否则从别的页切回来会先看到上一次的旧内容。
  renderView(v);
  if (v === 'keys' && typeof loadKeys === 'function') loadKeys();
  if (v === 'usage' && typeof loadUsage === 'function') loadUsage();
  if (v === 'requests' && typeof loadRequests === 'function') loadRequests();
  // 更新卡片不依赖 /api/state，先拉它：即使系统页的数据还没到也不会漏掉加载。
  if (v === 'system' && typeof loadUpdate === 'function') loadUpdate();
  // 重复推理保护开关同样不依赖 /api/state，进入系统页就拉当前运行期值。
  if (v === 'system' && typeof loadReasoningLoop === 'function') loadReasoningLoop();
  if (v === 'system' && !sysLoaded){ sysLoaded = true; renderSystem(); }
  if (v !== 'system' && typeof stopUpdatePoll === 'function') stopUpdatePoll();
  // 日志页：进入即拉一次，并按开关状态维持自动刷新；离开即停，避免后台空转。
  if (v === 'logs'){ loadLogs(); startLogAuto(); } else { stopLogAuto(); }
}
document.addEventListener('click', function(e){
  var b = e.target.closest('button[data-view]');
  if (b) go(b.getAttribute('data-view'));
});
$('#btnHome').addEventListener('click', function(){ go('overview'); });
$('#btnGoTasks').addEventListener('click', function(){ go('tasks'); });

/* ── 窄屏导航 ─────────────────────────────────────── */
function openNav(){ $('#side').classList.add('open'); $('#scrimNav').classList.add('on'); }
function closeNav(){ $('#side').classList.remove('open'); $('#scrimNav').classList.remove('on'); }
$('#btnMenu').addEventListener('click', openNav);
$('#scrimNav').addEventListener('click', closeNav);

/* ── 忙碌 ─────────────────────────────────────────── */
var busyDepth = 0;
function busy(on){
  busyDepth = Math.max(0, busyDepth + (on ? 1 : -1));
  var b = busyDepth > 0;
  ['#btnRefresh','#btnRestart','#btnAdd','#btnTaskReload','#btnLogs'].forEach(function(s){
    var el = $(s); if (el) el.disabled = b;
  });
}

/* ── 加载 ─────────────────────────────────────────── */
// CREDIT_RETRY 首次加载时积分还在后台查（后端返回 creditPending）的补取定时器。
var CREDIT_RETRY = null;

async function loadAll(forceCredit){
  var revision = ++S.loadRevision;
  var loadedAt = 0;
  busy(true);
  try{
    var res = await Promise.all([
      api('api/state' + (forceCredit ? '?refresh_credit=1' : '')).then(function(data){
        loadedAt = Date.now();
        return data;
      }),
      api('api/tasks').catch(function(){ return null; })
    ]);
    if (revision !== S.loadRevision) return;
    S.data = res[0];
    S.loadedAt = loadedAt;
    S.tasks = res[1];
    render();
    svc(true);
    scheduleCreditRetry(res[0]);
  }catch(err){
    if (revision !== S.loadRevision) return;
    svc(false, String(err && err.message || err));
    toast('加载失败：' + (err && err.message || err), 'err');
  }finally{ busy(false); }
}

// scheduleCreditRetry 后端说积分还在后台查时，过几秒自动补取一次；
// 这样首屏立刻可用，积分数字稍后自己出现，不必让页面干等（冷查约 8 秒）。
function scheduleCreditRetry(data){
  if (CREDIT_RETRY){ clearTimeout(CREDIT_RETRY); CREDIT_RETRY = null; }
  if (!data || !data.creditPending) return;
  CREDIT_RETRY = setTimeout(function(){
    CREDIT_RETRY = null;
    // 不传 force：走后端缓存优先路径，拿到后台线程写好的结果就渲染。
    loadAll();
  }, 3000);
}
function svc(up){
  var b = $('#svcBadge'), t = $('#svcText');
  var running = S.data && S.data.containerRunning;
  if (!up){ b.className = 'chip e'; t.textContent = '面板不可达'; return; }
  if (running){ b.className = 'chip ok'; t.textContent = '网关运行中'; }
  else { b.className = 'chip e'; t.textContent = '容器未运行'; }
}
function taskList(){
  return (S.tasks && S.tasks.available && Array.isArray(S.tasks.tasks)) ? S.tasks.tasks : [];
}
function badge(st){ return '<span class="bdg ' + st.cls + '"><i></i>' + st.text + '</span>'; }
function mrow(k, v, cls){
  return '<div class="mrow"><span class="k">' + esc(k) + '</span>' +
    '<span class="v ' + (cls || '') + '">' + esc(v) + '</span></div>';
}
function emptyBox(icon, title, sub){
  return '<div class="empty">' + svg(icon, 30) + '<div class="empty-t">' + esc(title) + '</div>' +
    (sub ? '<div class="empty-s">' + esc(sub) + '</div>' : '') + '</div>';
}

/* ── 渲染 ─────────────────────────────────────────── */
// render(view) 只重绘指定页；不传时重绘当前页。刷新（30 秒定时或手动）不该重建
// 用户没在看的页面：账号页最多几十行、日志页可达数百行，全部重建会让刷新瞬间掉帧。
// 侧栏计数与提示条是全局信息，每次刷新都更新。
function render(){
  var d = S.data; if (!d) return;
  if (d.service) $('#brandVer').textContent = d.service + (d.version ? ' · v' + d.version : '');
  var a = d.accounts || [], tasks = taskList();
  $('#tabAcct').textContent = a.length;
  $('#tabTask').textContent = tasks.length ? tasks.length : '—';
  renderNotes();
  renderStrip();
  renderView(CURRENT_VIEW);
}

// renderView 按页面分派；未列出渲染函数的页面保持原样（其数据由各自的 load 函数负责）。
function renderView(v){
  if (!S.data) return;
  if (v === 'overview'){ renderMetrics(); renderPool(); renderCredit(); renderOvTasks(); }
  else if (v === 'accounts'){ renderAccounts(); }
  else if (v === 'tasks'){ renderTasks(); }
  else if (v === 'system'){ renderSystem(); }
}

// 兼容旧调用点：仍可显式要求渲染全部页面（例如首次拿到数据后）。
function renderAll(){
  var d = S.data; if (!d) return;
  renderMetrics();
  renderPool();
  renderCredit();
  renderOvTasks();
  renderAccounts();
  renderTasks();
  renderSystem();
}

function metric(k, value, unit, sub, cls, pct, icon){
  return '<div class="metric">' +
    '<div class="k">' + esc(k) + '</div>' +
    '<div class="v">' + svg(icon, 15) + '<b>' + value + '</b>' +
    (unit ? '<small>' + esc(unit) + '</small>' : '') + '</div>' +
    (typeof pct === 'number'
      ? '<div class="bar"><i class="' + (cls || '') + '" style="width:' +
        Math.max(0, Math.min(100, pct)) + '%"></i></div>'
      : '') +
    (sub ? '<div class="s ' + (cls || '') + '">' + esc(sub) + '</div>' : '') +
    '</div>';
}

function renderMetrics(){
  var d = S.data, a = d.accounts || [], p = d.pool || {};
  var on = a.filter(function(x){ return !x.disabled; }).length;
  var total = Number(p.total) || 0, healthy = Number(p.healthy) || 0;
  var cooling = Number(p.cooling) || 0, dis = Number(p.disabled) || 0;
  var runs = taskList().filter(function(t){ return t.running; }).length;
  var have = a.filter(function(x){ return x.credits && typeof x.credits.remain === 'number'; });
  var remain = have.reduce(function(s, x){ return s + x.credits.remain; }, 0);
  var size = have.reduce(function(s, x){
    return s + (typeof x.credits.size === 'number' ? x.credits.size : x.credits.remain); }, 0);
  var pct = size ? Math.round(remain / size * 100) : 0;
  var hpct = total ? Math.round(healthy / total * 100) : 0;

  $('#metrics').innerHTML = [
    metric('账号', a.length, on + ' 启用', (a.length - on) + ' 个停用',
      (a.length - on) ? 'w' : 'ok', null, IC.users),
    metric('账号池', healthy + ' / ' + total, '健康',
      cooling ? (cooling + ' 个冷却中') : (dis ? (dis + ' 个停用') : '全部可用'),
      cooling ? 'w' : (dis ? 'e' : 'ok'), hpct, IC.pulse),
    metric('可用积分', num(remain), '剩余',
      have.length + ' / ' + a.length + ' 个账号取到',
      pct < 20 ? 'e' : (pct < 50 ? 'w' : 'ok'), pct, IC.coins),
    metric('排程', taskList().length ? String(taskList().length) : '—', '个任务',
      runs ? (runs + ' 个执行中') : '全部空闲', runs ? 'w' : '', null, IC.clock)
  ].join('');
}

function renderNotes(){
  var d = S.data, out = [];
  var off = (d.accounts || []).filter(function(a){ return a.disabled; });
  if (d.pendingRestart){
    out.push(note('w', '账号变更待生效',
      '有 ' + off.length + ' 个账号已停用但仍在池中，重启网关后生效。',
      { label:'立即重启', act:'restart' }));
  }
  if (!d.containerRunning){
    out.push(note('e', '网关容器未运行', '服务当前不可用，账号操作与积分查询都会失败。', null));
  }
  if (d.creditError){
    out.push(note('w', '积分数据不可用', String(d.creditError), null));
  }
  if (S.tasks && S.tasks.available === false){
    out.push(note('a', '排程未接入', '当前网关进程没有加载排程器。', null));
  }
  $('#notes').innerHTML = out.join('');
}
function note(kind, title, body, btn){
  var cls = kind === 'e' ? 'e' : (kind === 'w' ? 'w' : 'a');
  var icon = kind === 'a' ? IC.clock : IC.warn;
  return '<div class="note ' + cls + '">' + svg(icon, 16) +
    '<div class="g"><b>' + esc(title) + '</b> ' + esc(body) + '</div>' +
    (btn ? '<button class="btn sm" data-note-act="' + esc(btn.act) + '">' + esc(btn.label) + '</button>' : '') +
    '</div>';
}
$('#notes').addEventListener('click', function(e){
  var b = e.target.closest('[data-note-act]');
  if (b && b.getAttribute('data-note-act') === 'restart') actRestart();
});

function setCell(sel, text, copy){
  var el = $(sel), code = el.querySelector('code'), btn = el.querySelector('.mini');
  code.textContent = text;
  code.title = text;
  if (copy){ btn.disabled = false; btn.setAttribute('data-copy', copy); }
  else { btn.disabled = true; btn.removeAttribute('data-copy'); }
}
function renderStrip(){
  var base = location.origin + '/v1';
  var health = location.origin + '/healthz';
  setCell('#cellBase', base, base);
  setCell('#cellKey', '在密钥页管理', '');
  $('#cellKey .mini').disabled = false;
  setCell('#cellHealth', health, health);
}

function renderPool(){
  var p = S.data.pool || {};
  var total = p.total || 0, healthy = p.healthy || 0, cooling = p.cooling || 0, dis = p.disabled || 0;
  var off = Math.max(0, total - healthy - cooling - dis);
  function w(n){ return total ? (n / total * 100) : 0; }
  var html = '<div class="big"><span class="v">' + healthy + '</span><span class="u">/ ' + total + ' 健康</span></div>' +
    '<div class="comp">' +
      '<i class="a" style="width:' + w(healthy) + '%"></i>' +
      '<i class="b" style="width:' + w(cooling) + '%"></i>' +
      '<i class="c" style="width:' + w(dis + off) + '%"></i>' +
    '</div>' +
    mrow('池内总数', String(total)) +
    mrow('冷却中', String(cooling), cooling ? 'w' : '') +
    mrow('已停用', String(dis), dis ? 'w' : '') +
    mrow('并发占满', String(p.inFlightFull || 0)) +
    mrow('粘性会话', String(p.stickySessions || 0));
  var rt = p.realmTotals || {};
  Object.keys(rt).forEach(function(k){
    var v = rt[k], label = (k === 'cn' ? '国内' : (k === 'global' ? '国际' : k));
    if (v && typeof v === 'object'){
      var tot = Number(v.total) || 0, ok = Number(v.healthy) || 0;
      html += mrow(label + '可用', ok + ' / ' + tot, tot && ok === tot ? 'ok' : (ok ? '' : 'e'));
    } else {
      html += mrow(label + '可用', String(v));
    }
  });
  $('#poolPanel').innerHTML = html;
}

function renderCredit(){
  var d = S.data, a = d.accounts || [];
  var have = a.filter(function(x){ return x.credits && typeof x.credits.remain === 'number'; });
  // 积分在后台查询中（首屏不等 8 秒的冷查）：给出明确提示，稍后自动刷新。
  if (d.creditPending && !have.length){
    $('#creditPanel').innerHTML =
      '<div class="big"><span class="v">…</span><span class="u">查询中</span></div>' +
      mrow('状态', '正在后台统计积分') +
      mrow('取到积分', '0 / ' + a.length);
    return;
  }
  if (d.creditError && !have.length){
    $('#creditPanel').innerHTML =
      '<div class="big"><span class="v">—</span><span class="u">查询失败</span></div>' +
      mrow('原因', d.creditError);
    return;
  }
  // 按域汇总：国际与国内的积分是两套账（不同上游、不同计费），混在一起看不出
  // 哪边快用完。合计保留在最上面，下面分列，与「账号池」的 realmTotals 同一惯例。
  function sumOf(list){
    var remain = list.reduce(function(s, x){ return s + x.credits.remain; }, 0);
    var size = list.reduce(function(s, x){
      return s + (typeof x.credits.size === 'number' ? x.credits.size : x.credits.remain); }, 0);
    return {remain: remain, size: size, used: Math.max(0, size - remain), count: list.length};
  }
  var total = sumOf(have);
  var remain = total.remain, size = total.size, used = total.used;
  var pct = size ? Math.round(remain / size * 100) : 0;
  var html = '<div class="big"><span class="v">' + num(remain) + '</span><span class="u">剩余</span></div>' +
    '<div class="comp">' +
      '<i class="a" style="width:' + pct + '%"></i>' +
      '<i class="c" style="width:' + Math.max(0, 100 - pct) + '%"></i>' +
    '</div>' +
    mrow('已用', num(used)) +
    mrow('总额度', num(size)) +
    mrow('取到积分', have.length + ' / ' + a.length);
  // 分域小计：只列该域确实取到积分的账号；某域一个都没有时不显示，避免堆空行。
  [['global', '国际'], ['cn', '国内']].forEach(function(pair){
    var realm = pair[0], label = pair[1];
    var subset = have.filter(function(x){ return String(x.realm || '') === realm; });
    if (!subset.length) return;
    var part = sumOf(subset);
    var share = remain ? Math.round(part.remain / remain * 100) : 0;
    html += mrow(label + '剩余', num(part.remain) + '（占 ' + share + '% · ' + part.count + ' 个号）');
  });
  if (d.creditError) html += mrow('状态', '更新失败，显示上次结果');
  else if (d.creditPending) html += mrow('状态', '正在更新，显示上次结果');
  $('#creditPanel').innerHTML = html;
}

function taskState(t){
  if (!t) return { cls:'n', text:'未知' };
  if (!t.enabled) return { cls:'n', text:'已停用' };
  if (t.running) return { cls:'a', text:'执行中' };
  return { cls:'ok', text:'已启用' };
}
function sortTasks(list){
  return list.slice().sort(function(a, b){
    var ha = (a.hours && a.hours[0]) || 99, hb = (b.hours && b.hours[0]) || 99;
    return ha - hb;
  });
}
function renderOvTasks(){
  var list = taskList();
  if (!list.length){
    $('#ovTasks').innerHTML = emptyBox(IC.clock, '暂无排程任务', '网关进程没有加载排程器');
    return;
  }
  $('#ovTasks').innerHTML = sortTasks(list).map(function(t){
    var st = taskState(t);
    var time = (t.hours && t.hours.length) ? (pad(t.hours[0]) + ':00') : '—';
    return '<div class="tl-row">' +
      '<div class="tl-time">' + time + '</div>' +
      '<div class="tl-main"><div class="tl-nm">' + esc(t.name) + '</div>' +
      '<div class="tl-meta">下次 ' + esc(fmtTime(t.next_run)) + ' · 上次 ' + esc(relTime(t.last_run)) + '</div></div>' +
      '<div class="tl-acts">' + badge(st) +
      '<button class="btn sm" data-run="' + esc(t.key) + '">' + svg(IC.play, 11) + '跑一次</button>' +
      '<button class="btn sm ico ghost" data-log="' + esc(t.key) + '" data-name="' + esc(t.name) + '" title="日志">' +
      svg(IC.eye, 13) + '</button></div></div>';
  }).join('');
}

function acctState(a){
  if (a.disabled) return { cls:'n', text:'已停用' };
  var p = a.pool || {};
  if (!p.inPool) return { cls:'w', text:'不在池中' };
  if (p.cooling) return { cls:'w', text:'冷却中' };
  if (!p.healthy) return { cls:'e', text:'异常' };
  return { cls:'ok', text:'正常' };
}
function renderAccounts(){
  var all = S.data.accounts || [];
  var list = all.slice();
  var q = S.q.trim().toLowerCase();
  if (S.filter === 'on') list = list.filter(function(a){ return !a.disabled; });
  if (S.filter === 'off') list = list.filter(function(a){ return a.disabled; });
  if (q) list = list.filter(function(a){
    return (a.nickname || '').toLowerCase().indexOf(q) >= 0 ||
           (a.uid || '').toLowerCase().indexOf(q) >= 0;
  });
  if (!list.length){
    $('#acctRows').innerHTML = '<tr><td colspan="5" data-l="">' +
      emptyBox(IC.users, '没有匹配的账号', all.length ? '换个关键词或筛选条件' : '用右上角添加账号') +
      '</td></tr>';
    return;
  }
  $('#acctRows').innerHTML = list.map(function(a){
    var st = acctState(a), c = a.credits || {}, p = Object.assign({}, a.pool || {});
    var coolEnd = (S.loadedAt || Date.now()) + (Number(p.coolRemaining) || 0) * 1000;
    p.coolRemaining = Math.max(0, Math.ceil((coolEnd - Date.now()) / 1000));
    var left = daysLeft(a.expiresAt);
    var creditCell = (typeof c.remain === 'number')
      ? '<div class="cred"><span class="v">' + num(c.remain) + '</span>' +
        '<span class="m">已用 ' + num(c.used) + ' / ' + num(c.size) + '</span>' +
        (typeof c.size === 'number' && c.size > 0
          ? '<span class="bar"><i style="width:' + Math.min(100, Math.round(c.remain / c.size * 100)) + '%"></i></span>'
          : '') + '</div>'
      : '<span style="color:var(--ink-3)">—</span>';
    // 最近活动：成功优先，只有错误记录时明说「无成功」，避免与「无记录」混淆
    // （冷却中的号往往只有错误时间，此前两个时间戳都不写，整列显示「从未」）。
    var errAt = activityTime(p.lastErr), successAt = activityTime(p.lastSuccess);
    var lastTxt = successAt ? relTime(successAt) : (errAt ? '无成功记录' : '无记录');
    var lastSub = errAt ? ('最近错误 ' + relTime(errAt))
      : (p.breakerFails ? ('连错 ' + p.breakerFails + ' 次') : (p.inFlight ? ('并发 ' + p.inFlight) : ''));
    // 状态列：冷却中显示冷却剩余与原因；正常号才显示凭证有效期（两者混在一起会把
    // 「冷却中」误读成要等 363 天，见 coolLeft 注释）。
    // 冷却中的行带 data-cool-end（截止毫秒时间戳），交给 tickCooldowns 每秒就地刷新，
    // 不必等下一次整页数据拉取。
    var coolReasonTxt = coolReason(p);
    var coolSub = p.cooling
      ? '<span class="sub" data-cool-end="' + coolEnd +
        '" data-cool-reason="' + esc(coolReasonTxt) + '">' +
        esc(coolLeft(p.coolRemaining) + (coolReasonTxt ? ' · ' + coolReasonTxt : '')) + '</span>'
      : (left != null
        ? '<span class="sub">' + esc(left > 0 ? ('凭证剩 ' + left + ' 天') : '凭证已过期') + '</span>'
        : '');
    return '<tr>' +
      '<td data-l="账号"><div class="who">' + esc(a.nickname || '') + '</div>' +
      '<div class="sub mono">' + esc(shortUid(a.uid)) + ' · ' + esc(String(a.realm || '').toUpperCase()) + '</div></td>' +
      '<td data-l="状态"><span class="cell-in">' + badge(st) +
      coolSub +
      '</span></td>' +
      '<td class="r" data-l="积分"><span class="cred">' + creditCell + '</span></td>' +
      '<td data-l="最近活动"><span class="cell-in">' + esc(lastTxt) +
      (lastSub ? '<span class="sub">' + esc(lastSub) + '</span>' : '') + '</span></td>' +
      // 操作列：td 保持 table-cell（`class="r"` 与其它列一致地拿到 border-collapse
      // 的底边），flex 交给内层 div——td 自己 flex 化会让折叠边框不再绘制它。
      '<td class="r" data-l="操作"><div class="row-acts">' +
      '<button class="btn sm" data-toggle="' + esc(a.uid) + '" data-disabled="' + (a.disabled ? '1' : '0') + '">' +
      (a.disabled ? '启用' : '停用') + '</button>' +
      '<button class="btn sm ico ghost dgr" data-del="' + esc(a.uid) + '" data-name="' + esc(a.nickname || '') +
      '" title="删除">' + svg(IC.trash, 13) + '</button></div></td></tr>';
  }).join('');
}
// 搜索输入防抖：账号表在输入过程中每敲一个字都重建 DOM，几十行时能感到迟滞。
$('#acctSearch').addEventListener('input', debounce(function(e){ S.q = e.target.value; renderAccounts(); }, 120));
$('#acctFilter').addEventListener('click', function(e){
  var b = e.target.closest('button[data-f]'); if (!b) return;
  S.filter = b.getAttribute('data-f');
  $$('#acctFilter button').forEach(function(x){
    x.setAttribute('aria-pressed', String(x === b));
  });
  renderAccounts();
});
$('#acctRows').addEventListener('click', function(e){
  var t = e.target.closest('[data-toggle]');
  if (t) return actToggle(t.getAttribute('data-toggle'), t.getAttribute('data-disabled') === '0');
  var d = e.target.closest('[data-del]');
  if (d) return actDelete(d.getAttribute('data-del'), d.getAttribute('data-name'));
});

function renderTasks(){
  var list = taskList();
  if (!list.length){
    $('#taskList').innerHTML = emptyBox(IC.clock, '暂无排程任务',
      (S.tasks && S.tasks.error) || '网关进程没有加载排程器');
    return;
  }
  $('#taskList').innerHTML = sortTasks(list).map(function(t){
    var st = taskState(t);
    var hours = (t.hours || []).map(function(h){
      return '<span class="hour">' + pad(h) + ':00</span>'; }).join('');
    return '<div class="tile">' +
      '<div class="tile-t">' + badge(st) + '<span class="sp"></span>' +
      '<button class="sw" role="switch" data-task="' + esc(t.key) + '" aria-checked="' +
      (t.enabled ? 'true' : 'false') + '"' + (t.running ? ' disabled' : '') +
      ' aria-label="' + esc(t.name) + ' 开关"></button></div>' +
      '<div><div class="tl-nm">' + esc(t.name) + '</div>' +
      '<div class="detail">' + esc(t.detail || '') + '</div></div>' +
      '<div class="hours">' + hours + '</div>' +
      '<div class="tile-f">' +
      '<button class="btn sm" data-run="' + esc(t.key) + '">' + svg(IC.play, 11) + '跑一次</button>' +
      '<button class="btn sm ico ghost" data-log="' + esc(t.key) + '" data-name="' + esc(t.name) +
      '" title="日志">' + svg(IC.eye, 13) + '</button>' +
      '<span class="sp"></span><span class="meta">' + esc(fmtTime(t.next_run)) + '</span>' +
      '</div></div>';
  }).join('');
}

function renderSystem(){
  var d = S.data; if (!d) return;   // 直接打开 /#system（刷新或深链）时数据还没到，等 render() 再补
  var accounts = d.accounts || [];
  var cool = accounts.filter(function(a){ return a.pool && a.pool.cooling; }).length;
  var bad = accounts.filter(function(a){ return a.pool && a.pool.inPool && !a.pool.healthy; }).length;
  $('#sysCards').innerHTML =
    mrow('容器状态', d.containerRunning ? '运行中' : '已停止', d.containerRunning ? 'ok' : 'e') +
    mrow('容器名', 'workbuddy2api') +
    mrow('冷却账号', String(cool), cool ? 'w' : '') +
    mrow('异常账号', String(bad), bad ? 'e' : 'ok') +
    mrow('账号总数', String(accounts.length)) +
    mrow('排程任务', taskList().length ? String(taskList().length) : '—');
}

/* ── 交互委托 ─────────────────────────────────────── */
document.addEventListener('click', function(e){
  var r = e.target.closest('[data-run]');
  if (r){ actTaskRun(r.getAttribute('data-run')); return; }
  var g = e.target.closest('[data-log]');
  if (g){ openTaskLog(g.getAttribute('data-log'), g.getAttribute('data-name')); return; }
  var s = e.target.closest('[data-task]');
  if (s){ actTaskToggle(s.getAttribute('data-task'), s.getAttribute('aria-checked') !== 'true'); return; }
  var c = e.target.closest('[data-copy]');
  if (c) copyText(c.getAttribute('data-copy'));
});

function copyText(t){
  if (navigator.clipboard && navigator.clipboard.writeText){
    navigator.clipboard.writeText(t).then(function(){ toast('已复制', 'ok'); },
      function(){ toast('复制失败', 'err'); });
  } else {
    var ta = document.createElement('textarea');
    ta.value = t; document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); toast('已复制', 'ok'); }
    catch(err){ toast('复制失败', 'err'); }
    ta.remove();
  }
}

/* ── 确认弹窗 ─────────────────────────────────────── */
function ask(title, text, okLabel, danger, cb){
  $('#mdTitle').textContent = title;
  $('#mdText').textContent = text;
  $('#mdIcon').className = 'mic ' + (danger ? 'e' : 'w');
  var yes = $('#mdYes');
  yes.textContent = okLabel;
  modalCb = cb;
  $('#scrimM').classList.add('on');
  $('#modal').classList.add('on');
  setTimeout(function(){ yes.focus(); }, 60);
}
function closeModal(){
  $('#scrimM').classList.remove('on');
  $('#modal').classList.remove('on');
  modalCb = null;
}
$('#mdNo').addEventListener('click', closeModal);
$('#scrimM').addEventListener('click', closeModal);
$('#mdYes').addEventListener('click', function(){
  var cb = modalCb; closeModal(); if (cb) cb();
});
document.addEventListener('keydown', function(e){
  if (e.key !== 'Escape') return;
  if ($('#modal').classList.contains('on')) closeModal();
  else if ($('#logModal').classList.contains('on')) closeLogModal();
  else if ($('#drawer').classList.contains('on')) closeDrawer();
});

/* ── 操作 ─────────────────────────────────────────── */
async function actToggle(uid, nextDisabled){
  busy(true);
  try{
    var r = await api('api/account/toggle', { uid:uid, disabled:nextDisabled });
    toast(r.message, r.ok ? 'ok' : 'err');
    if (r.ok) await loadAll();
  }catch(err){ toast('操作失败：' + (err && err.message || err), 'err'); }
  finally{ busy(false); }
}
function actDelete(uid, name){
  ask('删除账号', '将把「' + (name || shortUid(uid)) + '」移入 auths-trash 并重启网关，可手动从回收目录恢复。',
    '删除', true, async function(){
      busy(true);
      try{
        var r = await api('api/account/delete', { uid:uid });
        toast(r.message, r.ok ? 'ok' : 'err');
        if (r.ok) await loadAll();
      }catch(err){ toast('删除失败：' + (err && err.message || err), 'err'); }
      finally{ busy(false); }
    });
}
function actRestart(){
  ask('重启网关', '重启期间服务会暂时中断，后台任务可能需要等待结束；账号与配置改动将在重新启动后生效。', '重启', false, async function(){
    busy(true);
    try{
      var r = await api('api/service/restart', {});
      toast(r.message, r.ok ? 'ok' : 'err');
      await loadAll(true);
    }catch(err){ toast('重启失败：' + (err && err.message || err), 'err'); }
    finally{ busy(false); }
  });
}
async function actTaskRun(key){
  busy(true);
  try{
    var r = await api('api/task/run', { key:key });
    toast(r.message, r.ok ? 'ok' : 'err');
    if (r.ok) setTimeout(function(){ loadAll(); }, 1200);
  }catch(err){ toast('触发失败：' + (err && err.message || err), 'err'); }
  finally{ busy(false); }
}
function actTaskToggle(key, nextEnabled){
  var t = taskList().filter(function(x){ return x.key === key; })[0] || {};
  ask((nextEnabled ? '启用' : '停用') + '任务',
    '「' + (t.name || key) + '」' + (nextEnabled ? '将恢复按点执行' : '将不再自动执行') + '，保存后重启网关生效。',
    nextEnabled ? '启用' : '停用', !nextEnabled, async function(){
      busy(true);
      try{
        var r = await api('api/task/toggle', { key:key, enabled:nextEnabled });
        toast(r.message, r.ok ? 'ok' : 'err');
        if (r.ok) await loadAll();
      }catch(err){ toast('操作失败：' + (err && err.message || err), 'err'); }
      finally{ busy(false); }
    });
}

/* ── 任务日志 ─────────────────────────────────────── */
function openLogModal(title){
  $('#lgTitle').textContent = title;
  $('#scrimL').classList.add('on');
  $('#logModal').classList.add('on');
}
function closeLogModal(){
  $('#scrimL').classList.remove('on');
  $('#logModal').classList.remove('on');
  logKey = null;
}
$('#lgClose').addEventListener('click', closeLogModal);
$('#scrimL').addEventListener('click', closeLogModal);
$('#lgRefresh').addEventListener('click', function(){ if (logKey) loadTaskLog(); });
async function openTaskLog(key, name){
  logKey = key; logName = name || key;
  openLogModal('任务日志 · ' + logName);
  $('#lgBody').textContent = '加载中…';
  await loadTaskLog();
}
async function loadTaskLog(){
  try{
    var r = await api('api/task/log?key=' + encodeURIComponent(logKey));
    var lines = r.lines || [];
    $('#lgBody').textContent = lines.length ? lines.join('\n') : (r.message || '这次运行还没有输出。');
  }catch(err){
    $('#lgBody').textContent = '拉取失败：' + (err && err.message || err);
  }
}

/* ── 请求日志（独立页，表格 + 自动刷新） ───────────── */
// sig 上一次渲染的内容指纹：自动刷新每 5 秒一次，几百行表格在内容没变时重建 DOM
// 纯属浪费，指纹一致就跳过重绘（只更新时间戳）。
var LOG = { loading:false, timer:null, auto:false, sig:'' };

function logAutoText(){
  var b = $('#btnLogAuto');
  if (!b) return;
  b.textContent = '自动刷新：' + (LOG.auto ? '开' : '关');
  b.setAttribute('aria-pressed', String(LOG.auto));
}

function startLogAuto(){
  if (!LOG.auto || LOG.timer) return;
  LOG.timer = setInterval(function(){
    if (document.visibilityState === 'visible') loadLogs();
  }, 5000);
}

function stopLogAuto(){
  if (LOG.timer){ clearInterval(LOG.timer); LOG.timer = null; }
}

function statusClass(code){
  var n = parseInt(code, 10);
  if (!n || n >= 500) return 'st-err';
  if (n >= 400) return 'st-warn';
  return 'st-ok';
}

async function loadLogs(){
  if (LOG.loading) return;
  var requestedLines = S.logLines;
  LOG.loading = true;
  var btn = $('#btnLogs');
  if (btn) btn.disabled = true;
  try{
    var r = await api('api/logs?lines=' + requestedLines);
    if (requestedLines !== S.logLines) return;
    var rows = r.rows || [];
    // 内容指纹一致时跳过重绘：自动刷新每 5 秒跑一次，没新请求就没必要重建几百行 DOM。
    var sig = requestedLines + '|' + JSON.stringify(rows);
    if (sig !== LOG.sig){
      LOG.sig = sig;
      if (!rows.length){
        $('#logRows').innerHTML = '<tr><td colspan="13"><div class="empty">还没有请求记录。</div></td></tr>';
      } else {
        $('#logRows').innerHTML = rows.slice().reverse().map(function(it){
          var cls = statusClass(it.status);
          return '<tr>' +
          '<td class="mono">#' + esc(it.seq) + '</td>' +
          '<td class="mono">' + esc(it.time) + '</td>' +
          '<td class="mono">' + esc(it.model) + '</td>' +
          '<td>' + esc(it.mode === 'stream' ? '流式' : '非流式') + '</td>' +
          '<td class="' + cls + '">' + esc(it.status) + '</td>' +
          '<td>' + esc(it.key) + '</td>' +
          '<td class="mono">' + esc(it.uid) + '</td>' +
          '<td class="mono num">' + esc(it.ttfb) + '</td>' +
          '<td class="mono num" title="输入 tokens（每轮重发整段上下文）">' + esc(compactNum(it.in)) + '</td>' +
          '<td class="mono num" title="输入里命中提示缓存的 tokens">' + esc(compactNum(it.hit)) + '</td>' +
          '<td class="mono num">' + esc(it.tok) + '</td>' +
          '<td class="mono num">' + esc(it.rate) + '</td>' +
          '<td class="mono num">' + esc(it.total) + '</td>' +
          '</tr>';
        }).join('');
      }
    }
    var count = $('#logCount'); if (count) count.textContent = String(rows.length);
    var updated = $('#logUpdated');
    if (updated){
      var now = new Date();
      function p(x){ return x < 10 ? '0' + x : '' + x; }
      updated.textContent = '更新于 ' + p(now.getHours()) + ':' + p(now.getMinutes()) + ':' + p(now.getSeconds());
    }
    $('#logBox').textContent = (r.other && r.other.length) ? r.other.join('\n') : '（无警告与错误）';
    var note = $('#logError');
    if (note){
      if (r.ok){ note.classList.add('hide'); }
      else { note.textContent = '读取容器日志失败，请检查面板是否有 docker 权限。'; note.classList.remove('hide'); }
    }
  }catch(err){
    if (requestedLines !== S.logLines) return;
    var note = $('#logError');
    if (note){ note.textContent = '拉取失败：' + (err && err.message || err); note.classList.remove('hide'); }
  }finally{
    LOG.loading = false;
    if (btn) btn.disabled = false;
    if (requestedLines !== S.logLines && CURRENT_VIEW === 'logs') await loadLogs();
  }
}

$('#logLines').addEventListener('click', function(e){
  var b = e.target.closest('button[data-n]'); if (!b) return;
  S.logLines = parseInt(b.getAttribute('data-n'), 10) || 120;
  $$('#logLines button').forEach(function(x){
    x.setAttribute('aria-pressed', String(x === b));
  });
  loadLogs();
});
$('#btnLogAuto').addEventListener('click', function(){
  LOG.auto = !LOG.auto;
  logAutoText();
  if (LOG.auto){ startLogAuto(); loadLogs(); } else { stopLogAuto(); }
});
logAutoText();

/* ── 添加账号 ─────────────────────────────────────── */
function openDrawer(){
  $('#scrimD').classList.add('on');
  $('#drawer').classList.add('on');
  renderPick();
}
function closeDrawer(){
  stopPoll();
  $('#scrimD').classList.remove('on');
  $('#drawer').classList.remove('on');
  DT.url = '';
}
$('#drClose').addEventListener('click', closeDrawer);
$('#scrimD').addEventListener('click', closeDrawer);
$('#btnAdd').addEventListener('click', openDrawer);

function renderPick(){
  DT.url = '';
  $('#drTitle').textContent = '添加账号';
  $('#drBody').innerHTML =
    '<div class="mtext">扫码完成授权后，凭证会写入 auths 并重启网关。</div>' +
    '<div style="margin-top:16px"><div class="sub" style="margin-bottom:7px">区域</div>' +
    '<div class="seg grow" id="segRealm">' +
    '<button data-r="cn" aria-pressed="' + (DT.realm === 'cn' ? 'true' : 'false') + '">国内版 CN</button>' +
    '<button data-r="global" aria-pressed="' + (DT.realm === 'global' ? 'true' : 'false') + '">国际版 Global</button>' +
    '</div></div>';
  $('#drFoot').innerHTML = '<button class="btn pri" id="drGo">生成授权链接</button>';
  $('#segRealm').addEventListener('click', function(e){
    var b = e.target.closest('button[data-r]'); if (!b) return;
    DT.realm = b.getAttribute('data-r');
    $$('#segRealm button').forEach(function(x){
      x.setAttribute('aria-pressed', String(x === b));
    });
  });
  $('#drGo').addEventListener('click', startLogin);
}

async function startLogin(){
  var go = $('#drGo'); go.disabled = true; go.textContent = '正在获取…';
  try{
    var r = await api('api/login/start', { realm:DT.realm });
    DT.loginId = r.login_id || '';
    if (!r.ok){ toast(r.message, 'err'); go.disabled = false; go.textContent = '重新生成'; return; }
    DT.url = r.url;
    renderWait();
    startPoll();
  }catch(err){
    toast('获取链接失败：' + (err && err.message || err), 'err');
    go.disabled = false; go.textContent = '重新生成';
  }
}

function renderWait(){
  $('#drTitle').textContent = '扫码登录';
  $('#drBody').innerHTML =
    '<div class="qr-box"><canvas id="qr"></canvas></div>' +
    '<div class="url-box" id="urlText">' + esc(DT.url) + '</div>' +
    '<div class="wait"><span class="spin"></span><span id="waitMsg">等待扫码…</span></div>';
  $('#drFoot').innerHTML = '<button class="btn" id="drCancel">重新选择区域</button>' +
    '<button class="btn" id="drCopy">复制链接</button>';
  $('#drCancel').addEventListener('click', renderPick);
  $('#drCopy').addEventListener('click', function(){ copyText(DT.url); });
  drawQR(DT.url);
}

function drawQR(text){
  var cv = $('#qr'); if (!cv) return;
  try{
    var q = qrcode(0, 'M');
    q.addData(text); q.make();
    var n = q.getModuleCount(), pad2 = 2;
    var cell = Math.max(2, Math.floor(176 / (n + pad2 * 2)));
    var side = cell * (n + pad2 * 2);
    var dpr = window.devicePixelRatio || 1;
    cv.width = side * dpr; cv.height = side * dpr;
    cv.style.width = side + 'px'; cv.style.height = side + 'px';
    var ctx = cv.getContext('2d');
    ctx.scale(dpr, dpr);
    ctx.fillStyle = '#fff'; ctx.fillRect(0, 0, side, side);
    ctx.fillStyle = '#111214';
    for (var r = 0; r < n; r++)
      for (var c = 0; c < n; c++)
        if (q.isDark(r, c)) ctx.fillRect((c + pad2) * cell, (r + pad2) * cell, cell, cell);
  }catch(err){
    cv.parentNode.innerHTML = '<div class="sub" style="padding:56px 0">二维码生成失败，请用下方链接打开</div>';
  }
}
function setWait(msg){ var w = $('#waitMsg'); if (w) w.textContent = msg; }
function stopPoll(){ if (DT.timer){ clearInterval(DT.timer); DT.timer = null; } }
function startPoll(){
  stopPoll(); DT.tries = 0;
  DT.timer = setInterval(async function(){
    if (DT.polling) return;
    if (DT.tries++ > 100){ stopPoll(); setWait('等待超时，请重新生成链接。'); return; }
    try{
      DT.polling = true;
      var r = await api('api/login/poll', { realm:DT.realm, login_id:DT.loginId });
      if (r.ok){ stopPoll(); renderDone(r); return; }
      if (r.pending && DT.tries % 3 === 0) setWait('等待扫码…');
    }catch(err){ /* 单次失败继续等 */ }
    finally { DT.polling = false; }
  }, 3000);
}
function renderDone(r){
  var a = r.account || {};
  $('#drTitle').textContent = '添加成功';
  $('#drBody').innerHTML =
    '<div class="note ok">' + svg(IC.check, 16) +
    '<div class="g"><b>' + esc(a.nickname || '账号') + '</b> 已写入并加载</div></div>' +
    '<div style="margin-top:14px">' +
    mrow('UID', a.uid || '—') +
    mrow('区域', String(a.realm || '').toUpperCase()) +
    mrow('有效期', a.expiresInDays ? (a.expiresInDays + ' 天') : '—') +
    '</div>';
  $('#drFoot').innerHTML = '<button class="btn" id="drMore">再加一个</button>' +
    '<button class="btn pri" id="drDone">完成</button>';
  $('#drDone').addEventListener('click', async function(){ closeDrawer(); await loadAll(true); });
  $('#drMore').addEventListener('click', renderPick);
  loadAll(true);
}

/* ── 顶栏动作 ─────────────────────────────────────── */
$('#btnRefresh').addEventListener('click', function(){
  loadAll(true);
  if (CURRENT_VIEW === 'keys' && typeof loadKeys === 'function') loadKeys();
  if (CURRENT_VIEW === 'usage' && typeof loadUsage === 'function') loadUsage();
  if (CURRENT_VIEW === 'requests' && typeof loadRequests === 'function') loadRequests(true);
  if (CURRENT_VIEW === 'logs') loadLogs();
});
$('#btnRestart').addEventListener('click', actRestart);
$('#btnTaskReload').addEventListener('click', async function(){
  await loadAll(); toast('已刷新', 'ok');
});
$('#btnLogs').addEventListener('click', loadLogs);
$('#btnLogout').addEventListener('click', function(){
  ask('退出登录', '将结束本机的登录会话，需要重新输入用户名和密码。', '退出', false, async function(){
    try{ await api('api/auth/logout', {}); }
    catch(e){ toast(e.message || '退出失败，请稍后重试', 'err'); return; }
    location.replace('login');
  });
});

/* ── 登录账号 ─────────────────────────────────────── */
async function loadIdentity(){
  try{
    var r = await api('api/session');
    $('#whoami').textContent = r.username || '';
    $('#pwUser').value = r.username || '';
    $('#pwHint').textContent = r.inherited
      ? '沿用 nginx 时期的密码，保存一次即切换成本面板自管'
      : '保存后所有设备都需要重新登录';
  }catch(err){ /* 401 已由 api() 跳转登录页 */ }
}
$('#btnPwSave').addEventListener('click', async function(){
  var cur = $('#pwCur').value, user = $('#pwUser').value.trim();
  var pw = $('#pwNew').value, cfm = $('#pwCfm').value;
  if (!cur){ toast('请输入当前密码', 'err'); $('#pwCur').focus(); return; }
  if (!user){ toast('请输入新用户名', 'err'); $('#pwUser').focus(); return; }
  if (pw.length < 8){ toast('新密码至少 8 位', 'err'); $('#pwNew').focus(); return; }
  if (pw !== cfm){ toast('两次输入的新密码不一致', 'err'); $('#pwCfm').focus(); return; }
  var btn = $('#btnPwSave');
  btn.disabled = true;
  try{
    var r = await api('api/auth/password',
      { current:cur, username:user, password:pw, confirm:cfm });
    toast(r.message || '已保存', 'ok');
    $('#pwCur').value = ''; $('#pwNew').value = ''; $('#pwCfm').value = '';
    $('#whoami').textContent = r.username || user;
  }catch(err){
    toast('保存失败：' + (err && err.message || err), 'err');
  }finally{ btn.disabled = false; }
});

/* ── 启动 ─────────────────────────────────────────── */
function init(){
  var v = 'overview';
  try { var s = localStorage.getItem('wb2a-view'); if (s && PAGE[s]) v = s; } catch(e){}
  var hashView = location.hash.slice(1); if (PAGE[hashView]) v = hashView;
  go(v);
  loadAll();
  loadIdentity();
  // 冷却倒计时按秒走；页面隐藏时 tick 仍在跑但没有任何可见成本，切回来即可对齐。
  startCoolTicker();
  setInterval(function(){
    if (!document.hidden && !DT.timer) loadAll();
  }, 30000);
}

// app.js 与 keys.js / usage.js 拼在同一个脚本里，后者的顶层状态（KS / US / LOG）
// 要等各自那一段执行完才赋值。init() 里的 go() 会直接调用这些页面的加载函数，
// 所以必须等整个脚本跑完再启动——否则从 #usage 进入时 US 还是 undefined，
// 页面会永远停在「正在加载用量…」。
if (document.readyState === 'loading'){
  document.addEventListener('DOMContentLoaded', init);
} else {
  init();
}
