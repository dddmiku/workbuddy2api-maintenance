"use strict";
// ═══ 更新日志 ═══
// 2026-09-25：逐密钥编辑请求频率、并发与排队，显示实时快照和请求明细入口；未知消费不补零。
// 2026-09-23：密钥新增「global 全部限流时回落同名 CN 模型」开关：表单、列表与提交
//             三处同步，未设置按关闭处理（既有密钥零回归）。
// 2026-09-20：密钥列表增加累计 token 用量列与有效期列；创建/编辑可选有效期，留空表示无限制。
// 2026-09-20：列表可再次复制完整密钥并单独切换重复推理保护；剪贴板失败时降级，关闭窗口立即清除明文。
// 2026-09-16：实现密钥创建、编辑、启停、删除与一次性显示，沿用控制台交互与主题。
// 2026-09-16：确认关闭时同步清空完整密钥，避免等待异步 close 事件才清除。
// 2026-09-17：密钥支持模型绑定：表单可填写或从模型列表挑选，列表展示绑定范围。

var KS = {keys:null, loading:false, error:'', query:'', models:null, modelItems:null, modelsLoading:false, modelQuery:'', guardDefault:true, revision:0, limits:null, limitsError:'', usageAvailable:true};
var KD = {id:null, secret:'', busy:false, copied:false, closeConfirmed:false, active:false, recoverable:false, mode:'edit'};

function keyGuardEnabled(key){
  return key && typeof key.reasoning_loop_guard === 'boolean' ? key.reasoning_loop_guard : KS.guardDefault;
}

// keyGlobalFallbackEnabled 读取密钥的 global→CN 回落开关。未设置（null/缺失）
// 一律按关闭处理：这是新增能力，不能让既有密钥因为字段缺失而改变行为。
function keyGlobalFallbackEnabled(key){
  return !!(key && key.global_fallback_to_cn === true);
}

function keyModels(value){
  return String(value || '').split(',').map(function(item){return item.trim();}).filter(function(item,index,all){
    return item && all.indexOf(item) === index;
  });
}

// keyExpiryLabel 把 expires_at 渲染成列表文案：无限制 / 剩余时间 / 已过期。
function keyExpiryLabel(key){
  if (!key || !key.expires_at) return {text:'无限制', cls:'key-expiry sub'};
  var when = new Date(key.expires_at);
  if (isNaN(when.getTime())) return {text:'无限制', cls:'key-expiry sub'};
  var left = when.getTime() - Date.now();
  if (left <= 0) return {text:'已过期', cls:'key-expiry key-expiry-expired'};
  var days = Math.floor(left / 86400000);
  var text = days >= 1 ? (days + ' 天后到期') : '不到 1 天到期';
  return {text:text, cls:'key-expiry sub', title:fmtTime(key.expires_at)};
}

// expiryToInput 把已有到期时间转成 datetime-local 的本地时间字符串。
function expiryToInput(iso){
  if (!iso) return '';
  var when = new Date(iso);
  if (isNaN(when.getTime())) return '';
  function pad(x){ return x < 10 ? '0' + x : '' + x; }
  return when.getFullYear() + '-' + pad(when.getMonth() + 1) + '-' + pad(when.getDate()) +
    'T' + pad(when.getHours()) + ':' + pad(when.getMinutes());
}

// selectedExpiry 读取表单里的有效期选择，返回 RFC3339 字符串或 null（无限制）。
// 校验失败时返回错误信息字符串，由调用方展示。
function selectedExpiry(){
  var mode = $('#keyExpiry').value;
  if (mode === 'none') return {value:null};
  if (mode === 'custom'){
    var raw = $('#keyExpiryCustom').value;
    if (!raw) return {error:'请选择到期时间，或把有效期改为「无限制」'};
    var when = new Date(raw);
    if (isNaN(when.getTime())) return {error:'到期时间格式不正确'};
    if (when.getTime() <= Date.now()) return {error:'到期时间需要晚于当前时间'};
    return {value:when.toISOString()};
  }
  var days = parseInt(mode, 10);
  if (!days || days <= 0) return {error:'请选择有效期'};
  return {value:new Date(Date.now() + days * 86400000).toISOString()};
}

async function loadKeyModels(){
  if (KS.models || KS.modelsLoading) return KS.models || [];
  KS.modelsLoading = true;
  try{
    var result = await api('api/models');
    KS.models = (result && Array.isArray(result.models)) ? result.models : [];
    // items 带展示字段（credits/name/vendor/能力旗标）；旧网关只回 models 纯 id 列表，
    // 那时降级成只有 id 的条目，面板仍可用（只是没有倍率徽标）。
    KS.modelItems = (result && Array.isArray(result.items) && result.items.length)
      ? result.items
      : KS.models.map(function(id){ return {id:id}; });
  }catch(error){ KS.models = []; KS.modelItems = []; }
  finally{
    KS.modelsLoading = false;
    renderModelPanel();
  }
  return KS.models;
}

// parseRate 把上游 credits 原文（"x0.79" / "x0.11 credits" / "0.06" / 空）解析成数字。
// 解析不出来返回 null——宁可不显示徽标，也不编一个倍率出来。
function parseRate(raw){
  if (raw === null || raw === undefined) return null;
  var m = String(raw).match(/(\d+(?:\.\d+)?)/);
  if (!m) return null;
  var n = parseFloat(m[1]);
  return isFinite(n) ? n : null;
}

// rateClass 按档位给徽标配色：免费（0）/ 低（<0.5）/ 高（≥0.5）。
function rateClass(n){
  if (n === null) return '';
  if (n <= 0) return 'free';
  return n < 0.5 ? 'low' : 'high';
}

function rateBadge(item){
  var n = parseRate(item && item.credits);
  if (n === null) return '<span class="mrate">倍率未知</span>';
  var text = n <= 0 ? '免费' : ('x' + (String(item.credits).match(/(\d+(?:\.\d+)?)/) || [,''])[1]);
  return '<span class="mrate ' + rateClass(n) + '">' + esc(text) + '</span>';
}

// modelMatches 判断条目是否命中关键词（模型名 / 显示名 / 供应商，多词全命中）。
function modelMatches(item, query){
  var terms = String(query || '').trim().toLowerCase().split(/\s+/).filter(Boolean);
  if (!terms.length) return true;
  var hay = [item.id, item.name, item.vendor].filter(Boolean).join(' ').toLowerCase();
  return terms.every(function(t){ return hay.indexOf(t) >= 0; });
}

// modelRank 给排序用的命中优先级（越小越靠前）：0 = 去掉 realm 前缀后的模型名
// 以关键词开头（打 "gl" 时 glm-* 排最前），1 = 模型名包含，2 = 只在完整 id
// 里命中（例如 "gl" 命中 global: 前缀）。这样既不隐藏任何相关项，又让最相关的
// 排在最上面——纯子串匹配会把 global:* 和 glm-* 混在一起，第一眼看不到想找的。
function modelRank(item, query){
  var terms = String(query || '').trim().toLowerCase().split(/\s+/).filter(Boolean);
  if (!terms.length) return 0;
  var bare = String(item.id || '').replace(/^(cn|global):/, '').toLowerCase();
  var name = String(item.name || '').toLowerCase();
  if (terms.every(function(t){ return bare.indexOf(t) === 0; })) return 0;
  if (terms.every(function(t){ return (bare + ' ' + name).indexOf(t) >= 0; })) return 1;
  return 2;
}

// filteredModelItems 返回当前关键词下的条目，按「相关度 → 已选中」排序。
function filteredModelItems(){
  var items = (KS.modelItems || []).filter(function(item){ return modelMatches(item, KS.modelQuery); });
  var chosen = currentModels();
  return items.slice().sort(function(a, b){
    var ra = modelRank(a, KS.modelQuery), rb = modelRank(b, KS.modelQuery);
    if (ra !== rb) return ra - rb;
    var ca = chosen.indexOf(a.id) >= 0 ? 0 : 1, cb = chosen.indexOf(b.id) >= 0 ? 0 : 1;
    if (ca !== cb) return ca - cb;
    return a.id < b.id ? -1 : (a.id > b.id ? 1 : 0);
  });
}

function renderModelPanel(){
  var panel = $('#keyModelPanel');
  if (!panel) return;
  var all = KS.modelItems || [];
  if (!all.length){
    panel.innerHTML = '<div class="mempty">' + (KS.modelsLoading ? '正在读取模型列表…' : '暂时读不到模型列表') + '</div>';
    updateModelCount(0, 0);
    return;
  }
  var items = filteredModelItems();
  updateModelCount(items.length, all.length);
  if (!items.length){
    panel.innerHTML = '<div class="mempty">没有匹配「' + esc(KS.modelQuery) + '」的模型</div>';
    return;
  }
  var chosen = currentModels();
  panel.innerHTML = items.map(function(item){
    var on = chosen.indexOf(item.id) >= 0;
    var caps = [];
    if (item.supports_images) caps.push('图片');
    if (item.supports_reasoning) caps.push('推理');
    if (item.supports_tool_call) caps.push('工具');
    return '<div class="mopt' + (on ? ' on' : '') + '" role="option" aria-selected="' + on + '" data-model="' + esc(item.id) + '">' +
      '<div class="mopt-main"><div class="mopt-id">' + esc(item.id) + '</div>' +
      '<div class="mopt-sub">' + (item.vendor ? '<span>' + esc(item.vendor) + '</span>' : '') +
      (caps.length ? '<span>' + esc(caps.join(' · ')) + '</span>' : '') + '</div></div>' +
      rateBadge(item) +
      '<span class="mopt-check">' + (on ? '✓' : '') + '</span></div>';
  }).join('');
}

// updateModelCount 显示「筛出 N / 共 M」，让用户知道列表被过滤了多少。
function updateModelCount(shown, total){
  var el = $('#keyModelCount');
  if (!el) return;
  el.textContent = (shown === total) ? '' : (shown + '/' + total);
}

function renderModelTags(){
  var box = $('#keyModelTags');
  if (!box) return;
  var items = KS.modelItems || [];
  var byId = {};
  items.forEach(function(item){ byId[item.id] = item; });
  box.innerHTML = currentModels().map(function(id){
    return '<span class="mtag">' + esc(id) + rateBadge(byId[id] || {}) +
      '<button type="button" class="mtag-x" data-drop-model="' + esc(id) + '" aria-label="移除 ' + esc(id) + '">×</button></span>';
  }).join('');
}

function currentModels(){
  var hidden = $('#keyModels');
  if (!hidden || !hidden.value) return [];
  return hidden.value.split(',').map(function(s){ return s.trim(); }).filter(Boolean);
}

function toggleModelPanel(open){
  var trigger = $('#keyModelPick'), panel = $('#keyModelPanel'), query = $('#keyModelQuery');
  if (!trigger || !panel) return;
  var next = open === undefined ? panel.hidden : open;
  panel.hidden = !next;
  trigger.classList.toggle('on', next);
  if (query) query.setAttribute('aria-expanded', next ? 'true' : 'false');
  if (next && query) query.focus();
}

// resetModelQuery 清空筛选词。打开编辑器/关闭下拉时调用，避免上一次的搜索词
// 让用户以为模型列表缺东西（面板被过滤成空看起来像「没有模型」）。
function resetModelQuery(){
  KS.modelQuery = '';
  var query = $('#keyModelQuery');
  if (query) query.value = '';
  renderModelPanel();
}

// setModelRow 就地更新下拉里某一行的选中态。
//
// 不能改成重建整个面板（renderModelPanel）：点击处理里重建 innerHTML 会让被点的
// 节点脱离 DOM，随后文档级的「点外面关闭」判定在该脱离节点上 closest('.mpick')
// 返回 null，把这次点击误判成点在外部而把面板关掉（2026-10-08 实测踩到）。
function setModelRow(id, on){
  var panel = $('#keyModelPanel');
  if (!panel) return;
  var row = panel.querySelector('[data-model="' + (window.CSS && CSS.escape ? CSS.escape(id) : id) + '"]');
  if (!row) return;
  row.classList.toggle('on', on);
  row.setAttribute('aria-selected', on ? 'true' : 'false');
  var check = row.querySelector('.mopt-check');
  if (check) check.textContent = on ? '✓' : '';
}

function fillModelInput(models){
  $('#keyModels').value = models.join(', ');
  renderModelTags();
}

async function loadKeys(){
  if (KS.loading) return;
  var revision = KS.revision, reload = false;
  KS.loading = true;
  $('#btnKeysReload').disabled = true;
  try{
    var results = await Promise.all([
      api('api/keys'),
      api('api/key-limits').then(function(value){return {value:value};}).catch(function(error){return {error:error};})
    ]);
    if (revision !== KS.revision){ reload = true; return; }
    var result = results[0], status = results[1];
    if (!result || !Array.isArray(result.keys)) throw new Error('服务返回的密钥列表不完整');
    KS.keys = result.keys; KS.error = '';
    KS.usageAvailable = result.usage_available !== false;
    KS.limits = status.value && status.value.ok === true && status.value.keys && typeof status.value.keys === 'object' ? status.value.keys : null;
    KS.limitsError = KS.limits ? '' : '暂时无法读取限流占用，已保存的限流设置仍然生效。';
    if (typeof result.default_reasoning_loop_guard === 'boolean') KS.guardDefault = result.default_reasoning_loop_guard;
  }catch(error){ if (revision === KS.revision) KS.error = error.message || '加载失败，请稍后重试'; else reload = true; }
  finally{
    KS.loading = false; $('#btnKeysReload').disabled = false; renderKeys();
    // A refresh begun before a saved change must not restore stale switches.
    if (reload) loadKeys();
  }
}

function keyLimitsSummary(key){
  var policy = key.limits || {}, rpm = policy.requests_per_minute || 0, concurrent = policy.max_concurrent || 0, queue = policy.queue_timeout_seconds || 0;
  var text = (rpm ? rpm + ' 次/分' : '不限频率') + ' · ' + (concurrent ? '并发 ' + concurrent : '不限并发');
  var rows = '<div>' + esc(text) + '</div>';
  if (concurrent) rows += '<div class="sub">' + (queue ? '最多等待 ' + queue + ' 秒' : '并发满时不排队') + '</div>';
  if (rpm || concurrent){
    var status = KS.limits && KS.limits[key.id], observed = [];
    if (status && status.policy && status.policy.requests_per_minute === rpm && status.policy.max_concurrent === concurrent && status.policy.queue_timeout_seconds === queue){
      if (rpm && Number.isSafeInteger(status.requests)) observed.push('窗口内 ' + status.requests + ' 次');
      if (concurrent && Number.isSafeInteger(status.active) && Number.isSafeInteger(status.queued)) observed.push('在途 ' + status.active + ' · 排队 ' + status.queued);
    }
    rows += '<div class="sub key-limit-state">' + esc(observed.join(' · ') || '占用待刷新') + '</div>';
  }
  return rows;
}

function selectedKeyLimits(){
  var fields = [['keyRPM','requests_per_minute',60000],['keyConcurrency','max_concurrent',256],['keyQueueSeconds','queue_timeout_seconds',30]], policy = {};
  for (var i=0;i<fields.length;i++){
    var field=fields[i], raw=$('#'+field[0]).value.trim(), value=raw==='' ? 0 : Number(raw);
    if (!/^\d*$/.test(raw) || !Number.isSafeInteger(value) || value<0 || value>field[2]) return {error:'限流设置需要填写 0–' + field[2] + ' 的整数', field:field[0]};
    policy[field[1]]=value;
  }
  if (policy.queue_timeout_seconds>0 && !policy.max_concurrent) return {error:'请先设置并发上限，再设置等待时间',field:'keyConcurrency'};
  return {value:policy.requests_per_minute || policy.max_concurrent ? policy : null};
}

function renderKeys(){
  var keys = KS.keys || [];
  $('#keyError').textContent = KS.error;
  $('#keyError').classList.toggle('hide', !KS.error);
  var messages = [KS.limitsError, KS.usageAvailable ? '' : '累计用量暂不可读，当前显示为“—”，恢复后刷新即可。'].filter(Boolean).join(' ');
  $('#keyLimitsError').textContent = messages;
  $('#keyLimitsError').classList.toggle('hide', !messages);
  $('#tabKeys').textContent = KS.keys ? keys.length : '—';
  $('#btnCreateKey').disabled = KS.keys === null;
  $('#keyEnabledCount').textContent = KS.keys ? keys.filter(function(k){return k.enabled;}).length : '—';
  var query = KS.query.toLowerCase();
  var filtered = keys.filter(function(k){return (k.name + ' ' + (k.note || '')).toLowerCase().indexOf(query) >= 0;});
  if (!filtered.length){
    var message = !KS.keys ? (KS.error ? '暂时无法加载密钥' : '正在加载密钥…') : (query ? '没有匹配的密钥' : '还没有密钥');
    $('#keyRows').innerHTML = '<tr><td colspan="8">' + emptyBox(IC.box, message, !query && KS.keys ? '创建一把密钥，用于连接你的客户端。' : '') + '</td></tr>';
    return;
  }
  $('#keyRows').innerHTML = filtered.map(function(key){
    var models = keyModels((key.models || []).join(','));
    var expiry = keyExpiryLabel(key);
    return '<tr><td data-l="名称"><div class="key-name">' + esc(key.name) + (key.legacy ? '<span class="key-legacy">原有</span>' : '') + '</div>' +
      '<div class="sub key-note">' + esc(key.note || '未填写备注') + '</div></td>' +
      '<td data-l="密钥"><div class="key-value"><code class="key-mask">' + esc(key.masked_key) + '</code>' +
      '<button type="button" class="btn sm" data-key-action="copy" data-id="' + esc(key.id) + '" title="' +
      (key.copy_available ? '复制完整密钥' : '旧密钥正常使用后可再次复制') + '">复制</button></div></td>' +
      '<td data-l="模型绑定">' + (models.length
        ? '<div class="key-model-tags">' + models.map(function(name){return '<span class="key-model-tag">' + esc(name) + '</span>';}).join('') + '</div>'
        : '<span class="sub">不限制</span>') + '</td>' +
      '<td data-l="总用量" class="mono key-tokens">' + (typeof key.total_tokens === 'number' && Number.isFinite(key.total_tokens) && key.total_tokens >= 0 ? esc(compactTokens(key.total_tokens)) : '<span class="sub" title="累计用量暂不可读">—</span>') + '</td>' +
      '<td data-l="限流与占用" class="key-limits"><div class="key-limit-content">' + keyLimitsSummary(key) + '<button class="btn sm" data-key-action="limits" data-id="' + esc(key.id) + '">设置限流</button></div></td>' +
      '<td data-l="状态与保护" class="key-state"><span class="bdg ' + (key.enabled ? 'ok' : 'off') + '"><i></i>' + (key.enabled ? '启用' : '停用') + '</span>' +
      '<div class="key-state-toggles">' +
      '<button type="button" class="btn sm key-guard-toggle" role="switch" aria-checked="' + keyGuardEnabled(key) +
      '" aria-label="' + esc(key.name) + '的重复推理保护" data-key-action="guard" data-id="' + esc(key.id) +
      '" title="发现持续重复输出（推理或正文）时结束该次请求；只影响此密钥后续请求">推理保护 ' + (keyGuardEnabled(key) ? '开' : '关') + '</button>' +
      '<button type="button" class="btn sm key-guard-toggle" role="switch" aria-checked="' + keyGlobalFallbackEnabled(key) +
      '" aria-label="' + esc(key.name) + '的 CN 回落" data-key-action="fallback" data-id="' + esc(key.id) +
      '" title="global 号全部被限流时，改用同名 CN 模型继续；只影响此密钥后续请求">CN 回落 ' + (keyGlobalFallbackEnabled(key) ? '开' : '关') + '</button>' +
      '</div></td>' +
      // 有效期与创建时间合成一列：两者都是时间戳，语义连贯，竖排后可省下一整列的
      // 横向空间（2026-10-09：9 列在 1440 视口下仍溢出 113px，用户实测右侧操作列被切）。
      '<td data-l="有效期与创建"><div class="key-when"><span class="' + expiry.cls + '"' + (expiry.title ? ' title="' + esc(expiry.title) + '"' : '') + '>' + esc(expiry.text) + '</span>' +
      '<span class="sub mono">创建 ' + esc(fmtTime(key.created_at)) + '</span></div></td>' +
      '<td data-l="操作"><div class="key-actions"><button class="btn sm" data-key-action="requests" data-id="' + esc(key.id) + '">请求明细</button><button class="btn sm" data-key-action="edit" data-id="' + esc(key.id) + '">编辑</button>' +
      '<button class="btn sm" data-key-action="toggle" data-id="' + esc(key.id) + '">' + (key.enabled ? '停用' : '启用') + '</button>' +
      '<button class="btn sm danger" data-key-action="delete" data-id="' + esc(key.id) + '">删除</button></div></td></tr>';
  }).join('');
}

function keyFormError(message){
  $('#keyFormError').textContent = message;
  $('#keyFormError').classList.toggle('hide', !message);
}

function openKeyEditor(key){
  KD = {id:key ? key.id : null, secret:'', busy:false, copied:false, closeConfirmed:false, active:true, recoverable:false, mode:'edit'};
  $('#keyDialogTitle').textContent = key ? '编辑密钥' : '创建密钥';
  $('#keyName').value = key ? key.name : '';
  $('#keyNote').value = key ? (key.note || '') : '';
  fillModelInput(key ? keyModels((key.models || []).join(',')) : []);
  $('#keyGuard').checked = keyGuardEnabled(key);
  $('#keyGlobalFallback').checked = keyGlobalFallbackEnabled(key);
  var limits = key && key.limits || {};
  $('#keyRPM').value = limits.requests_per_minute || 0;
  $('#keyConcurrency').value = limits.max_concurrent || 0;
  $('#keyQueueSeconds').value = limits.queue_timeout_seconds || 0;
  // 有效期默认「无限制」：既有密钥和新密钥都保持这个默认，只有显式选择才设置。
  // 编辑一把已经设过有效期的密钥时回填原值，避免保存时把有效期无声清掉。
  var preset = key && key.expires_at ? expiryToInput(key.expires_at) : '';
  $('#keyExpiry').value = preset ? 'custom' : 'none';
  $('#keyExpiryCustom').value = preset;
  $('#keyExpiryCustomField').classList.toggle('hide', !preset);
  toggleModelPanel(false);
  resetModelQuery();
  $('#keySecret').value = '';
  $('#keyFields').classList.remove('hide'); $('#keyCreated').classList.add('hide');
  $('#keyDialogSave').classList.remove('hide'); $('#keyDialogSave').textContent = key ? '保存修改' : '创建密钥';
  $('#keyDialogSave').disabled = false; $('#keyDialogCancel').textContent = '取消';
  $('#btnCopyKey').textContent = '复制密钥';
  $('#btnCopyKey').disabled = false;
  $('#keySecretStatus').textContent = '密钥已创建，可以用于连接网关。';
  keyFormError('');
  $('#keyDialog').showModal(); $('#keyName').focus();
  loadKeyModels();
}

function closeKeyEditor(){
  if (KD.busy && KD.mode !== 'copy') return;
  if (KD.secret && !KD.recoverable && !KD.copied && !KD.closeConfirmed){
    KD.closeConfirmed = true;
    keyFormError('请确认已保存密钥。继续关闭后，无法再次查看完整密钥。');
    $('#keyDialogCancel').textContent = '已保存，关闭';
    return;
  }
  KD.active = false; KD.secret = ''; $('#keySecret').value = '';
  $('#keyDialog').close();
}

$('#keyDialog').addEventListener('close', function(){
  if ($('#keyDialog').open) return;
  KD.active = false; KD.secret = ''; $('#keySecret').value = ''; $('#keyName').value = ''; $('#keyNote').value = ''; $('#keyModels').value = '';
});
$('#keyDialog').addEventListener('cancel', function(event){ event.preventDefault(); closeKeyEditor(); });
$('#keyDialogClose').addEventListener('click', closeKeyEditor);
$('#keyDialogCancel').addEventListener('click', closeKeyEditor);
$('#btnCreateKey').addEventListener('click', function(){openKeyEditor(null);});
$('#btnKeysReload').addEventListener('click', loadKeys);
$('#keySearch').addEventListener('input', function(){KS.query=this.value;renderKeys();});
// 可输入筛选的模型选择器：整条是搜索框，输入即过滤（打 "gl" 只剩 glm-*），
// 点一行加入绑定（再点取消），标签上的 × 移除。点击面板外或按 Esc 关闭。
$('#keyModelPick').addEventListener('click', function(){ toggleModelPanel(true); });
$('#keyModelQuery').addEventListener('input', function(){
  KS.modelQuery = this.value;
  toggleModelPanel(true);
  renderModelPanel();
});
$('#keyModelQuery').addEventListener('focus', function(){ toggleModelPanel(true); });
// 键盘：下箭头进列表，Esc 关面板（不冒泡给对话框）。
$('#keyModelQuery').addEventListener('keydown', function(event){
  if (event.key === 'ArrowDown'){
    event.preventDefault();
    var first = $('#keyModelPanel').querySelector('.mopt');
    if (first) first.scrollIntoView({block:'nearest'});
  } else if (event.key === 'Escape'){
    event.stopPropagation();
    toggleModelPanel(false);
  }
});
$('#keyModelPanel').addEventListener('click', function(event){
  var row = event.target.closest('[data-model]');
  if (!row || row.classList.contains('dis')) return;
  var id = row.getAttribute('data-model');
  var models = currentModels();
  var at = models.indexOf(id);
  if (at >= 0){ models.splice(at, 1); setModelRow(id, false); }
  else { models.push(id); setModelRow(id, true); }
  $('#keyModels').value = models.join(', ');
  renderModelTags();
  $('#keyModelQuery').focus();
});
$('#keyModelTags').addEventListener('click', function(event){
  var btn = event.target.closest('[data-drop-model]');
  if (!btn) return;
  var models = currentModels();
  var at = models.indexOf(btn.getAttribute('data-drop-model'));
  if (at >= 0){ models.splice(at, 1); fillModelInput(models); }
});
document.addEventListener('click', function(event){
  if (!event.target.closest('.mpick') && !event.target.closest('#keyModelTags')) toggleModelPanel(false);
});
document.addEventListener('keydown', function(event){
  if (event.key === 'Escape') toggleModelPanel(false);
});
$('#btnClearModels').addEventListener('click', function(){ fillModelInput([]); });
$('#keyExpiry').addEventListener('change', function(){
  $('#keyExpiryCustomField').classList.toggle('hide', this.value !== 'custom');
  if (this.value === 'custom') $('#keyExpiryCustom').focus();
});

$('#keyForm').addEventListener('submit', async function(event){
  event.preventDefault(); if (KD.busy || KD.secret) return;
  var body = {name:$('#keyName').value.trim(), note:$('#keyNote').value.trim(), models:keyModels($('#keyModels').value), reasoning_loop_guard:$('#keyGuard').checked, global_fallback_to_cn:$('#keyGlobalFallback').checked};
  if (!body.name){keyFormError('请填写密钥名称');$('#keyName').focus();return;}
  if (body.models.length > 64){keyFormError('模型绑定最多 64 项');$('#keyModels').focus();return;}
  if (body.models.some(function(item){return item.length > 64;})){keyFormError('单个模型名不能超过 64 个字符');$('#keyModels').focus();return;}
  // 绑定按完整模型名逐字比对，裸名一条也匹配不上（模型列表里没有裸名）。
  // 手填时先在这里拦住，免得保存成功却调用全 403。
  if (body.models.some(function(item){return !/^(cn|global):.+/.test(item);})){
    keyFormError('模型绑定必须选完整模型名（cn: 或 global: 开头），请从模型列表添加');$('#keyModels').focus();return;
  }
  var expiry = selectedExpiry();
  if (expiry.error){keyFormError(expiry.error);return;}
  body.expires_at = expiry.value;
  var limits = selectedKeyLimits();
  if (limits.error){keyFormError(limits.error);$('#'+limits.field).focus();return;}
  body.limits = limits.value;
  if (KD.id) body.id = KD.id;
  KD.busy = true; $('#keyDialogSave').disabled = true; keyFormError('');
  try{
    var result = await api(KD.id ? 'api/keys/update' : 'api/keys', body);
    KS.revision++;
    if (KD.id){ KD.busy = false; closeKeyEditor(); toast('密钥信息已保存','ok'); }
    else{
      if (typeof result.key !== 'string' || !result.key || result.key.length > 512) throw new Error('未收到完整密钥，请刷新列表后重试');
      KD.secret = result.key;
      KD.recoverable = !!(result.entry && result.entry.copy_available);
      $('#keySecret').value = KD.secret;
      $('#keyFields').classList.add('hide'); $('#keyCreated').classList.remove('hide');
      $('#keyDialogSave').classList.add('hide'); $('#keyDialogCancel').textContent = '完成';
      $('#keyDialogTitle').textContent = '保存你的密钥'; $('#btnCopyKey').focus();
      $('#keySecretHint').textContent = KD.recoverable ? '可以现在复制，也可以关闭后从密钥列表再次复制。' : '请现在复制并保存，当前服务尚不支持再次读取。';
      KD.busy = false;
    }
    await loadKeys();
  }catch(error){keyFormError(error.message || '保存失败，请稍后重试');}
  finally{KD.busy = false;$('#keyDialogSave').disabled = false;}
});

async function copyCurrentKey(){
  var state = KD, secret = state.secret;
  if (!secret || !state.active) return false;
  var copied = false;
  try{
    if (navigator.clipboard && window.isSecureContext){ await navigator.clipboard.writeText(secret); copied = true; }
  }catch(error){}
  if (KD !== state || !state.active || state.secret !== secret) return copied;
  var field = $('#keySecret');
  if (!copied){
    field.focus(); field.select(); field.setSelectionRange(0, secret.length);
    try{ copied = !!document.execCommand('copy'); }catch(error){}
  }
  if (copied){
    state.copied = true; keyFormError(''); $('#btnCopyKey').textContent = '已复制'; toast('密钥已复制','ok');
  }else{
    keyFormError('已选中完整密钥，请按 ctrl + c 或长按复制。');
    field.focus(); field.select(); field.setSelectionRange(0, secret.length);
  }
  return copied;
}

$('#btnCopyKey').addEventListener('click', copyCurrentKey);

async function copySavedKey(key){
  var state = {id:null, secret:'', busy:true, copied:false, closeConfirmed:false, active:true, recoverable:true, mode:'copy'};
  KD = state;
  $('#keyDialogTitle').textContent = '复制「' + key.name + '」的密钥';
  $('#keyFields').classList.add('hide'); $('#keyCreated').classList.remove('hide');
  $('#keySecret').value = ''; $('#keySecretStatus').textContent = '正在读取密钥…';
  $('#keySecretHint').textContent = '复制完成后可关闭窗口，列表里仍能再次复制。';
  $('#keyDialogSave').classList.add('hide'); $('#keyDialogCancel').textContent = '关闭';
  $('#btnCopyKey').textContent = '复制密钥'; $('#btnCopyKey').disabled = true;
  keyFormError(''); $('#keyDialog').showModal();
  try{
    var result = await api('api/keys/copy', {id:key.id});
    if (KD !== state || !state.active) return;
    if (typeof result.key !== 'string' || !result.key || result.key.length > 512) throw new Error('未获取完整密钥，请刷新列表后重试');
    state.secret = result.key; state.busy = false;
    $('#keySecret').value = result.key; $('#keySecretStatus').textContent = '已读取完整密钥。';
    $('#btnCopyKey').disabled = false;
    await copyCurrentKey();
  }catch(error){
    if (KD === state && state.active){ $('#keySecretStatus').textContent = '暂时无法复制这把密钥。'; keyFormError(error.message || '读取失败，请稍后重试'); }
  }finally{state.busy = false;}
}

$('#keyRows').addEventListener('click', function(event){
  var button = event.target.closest('button[data-key-action]'); if (!button) return;
  var key = (KS.keys || []).find(function(k){return k.id === button.getAttribute('data-id');}); if (!key) return;
  var action = button.getAttribute('data-key-action');
  if (action === 'edit'){openKeyEditor(key);return;}
  if (action === 'limits'){openKeyEditor(key);$('#keyRPM').focus();return;}
  if (action === 'requests'){if (typeof openRequestsForKey === 'function') openRequestsForKey(key.id,key.name);return;}
  if (action === 'copy'){copySavedKey(key);return;}
  if (action === 'guard'){
    var desired = !keyGuardEnabled(key); button.disabled = true;
    api('api/keys/update', {id:key.id, reasoning_loop_guard:desired}).then(function(result){
      KS.revision++;
      if (result.entry && result.entry.id === key.id){
        KS.keys = KS.keys.map(function(item){return item.id === key.id ? result.entry : item;}); renderKeys();
      }
      toast('重复推理保护已' + (desired ? '开启' : '关闭') + '，对后续请求生效','ok');
      return loadKeys();
    }).catch(function(error){toast(error.message || '设置失败','err');}).finally(function(){button.disabled = false;});
    return;
  }
  if (action === 'fallback'){
    var wantFallback = !keyGlobalFallbackEnabled(key); button.disabled = true;
    api('api/keys/update', {id:key.id, global_fallback_to_cn:wantFallback}).then(function(result){
      KS.revision++;
      if (result.entry && result.entry.id === key.id){
        KS.keys = KS.keys.map(function(item){return item.id === key.id ? result.entry : item;}); renderKeys();
      }
      toast('CN 回落已' + (wantFallback ? '开启' : '关闭') + '，对后续请求生效','ok');
      return loadKeys();
    }).catch(function(error){toast(error.message || '设置失败','err');}).finally(function(){button.disabled = false;});
    return;
  }
  var removing = action === 'delete';
  var label = removing ? '删除' : (key.enabled ? '停用' : '启用');
  var message = removing ? '删除后无法恢复。使用这把密钥的客户端将无法继续发起请求。' : (key.enabled ? '使用这把密钥的客户端将无法发起新请求，之后可以重新启用。' : '启用后，这把密钥可以重新用于调用网关。');
  if (key.enabled && (removing || action === 'toggle') && KS.keys.filter(function(k){return k.enabled;}).length === 1) message += ' 这是最后一把启用的密钥；你仍可从管理面板创建新密钥。';
  ask(label + '「' + key.name + '」',message,label,removing || key.enabled,async function(){
    button.disabled = true;
    try{
      await api(removing ? 'api/keys/delete' : 'api/keys/update',removing ? {id:key.id} : {id:key.id,enabled:!key.enabled});
      KS.revision++;
      toast('密钥已' + label,'ok'); await loadKeys();
    }catch(error){toast(error.message || '操作失败','err');}
    finally{button.disabled = false;}
  });
});

// 注意：这里不要调用 init()。keys.js 与 usage.js 会被拼进同一个脚本，本段执行时
// usage.js 的顶层状态（US）还没赋值，从 #usage 进入就会抛
// "Cannot read properties of undefined (reading 'loading')" 并卡在加载态。
// 启动统一由 app.js 末尾负责，且延迟到整个脚本执行完之后。
