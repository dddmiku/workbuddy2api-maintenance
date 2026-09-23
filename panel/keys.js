"use strict";
// ═══ 更新日志 ═══
// 2026-09-23：密钥新增「global 全部限流时回落同名 CN 模型」开关：表单、列表与提交
//             三处同步，未设置按关闭处理（既有密钥零回归）。
// 2026-09-20：密钥列表增加累计 token 用量列与有效期列；创建/编辑可选有效期，留空表示无限制。
// 2026-09-20：列表可再次复制完整密钥并单独切换重复推理保护；剪贴板失败时降级，关闭窗口立即清除明文。
// 2026-09-16：实现密钥创建、编辑、启停、删除与一次性显示，沿用控制台交互与主题。
// 2026-09-16：确认关闭时同步清空完整密钥，避免等待异步 close 事件才清除。
// 2026-09-17：密钥支持模型绑定：表单可填写或从模型列表挑选，列表展示绑定范围。

var KS = {keys:null, loading:false, error:'', query:'', models:null, guardDefault:true, revision:0};
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
  }catch(error){ KS.models = []; }
  finally{
    KS.modelsLoading = false;
    var pick = $('#keyModelPick');
    pick.innerHTML = '<option value="">从模型列表添加…</option>' + KS.models.map(function(id){
      return '<option value="' + esc(id) + '">' + esc(id) + '</option>';
    }).join('');
  }
  return KS.models;
}

function fillModelInput(models){
  $('#keyModels').value = models.join(', ');
}

async function loadKeys(){
  if (KS.loading) return;
  var revision = KS.revision, reload = false;
  KS.loading = true;
  $('#btnKeysReload').disabled = true;
  try{
    var result = await api('api/keys');
    if (revision !== KS.revision){ reload = true; return; }
    if (!result || !Array.isArray(result.keys)) throw new Error('服务返回的密钥列表不完整');
    KS.keys = result.keys; KS.error = '';
    if (typeof result.default_reasoning_loop_guard === 'boolean') KS.guardDefault = result.default_reasoning_loop_guard;
  }catch(error){ if (revision === KS.revision) KS.error = error.message || '加载失败，请稍后重试'; else reload = true; }
  finally{
    KS.loading = false; $('#btnKeysReload').disabled = false; renderKeys();
    // A refresh begun before a saved change must not restore stale switches.
    if (reload) loadKeys();
  }
}

function renderKeys(){
  var keys = KS.keys || [];
  $('#keyError').textContent = KS.error;
  $('#keyError').classList.toggle('hide', !KS.error);
  $('#tabKeys').textContent = KS.keys ? keys.length : '—';
  $('#btnCreateKey').disabled = KS.keys === null;
  $('#keyEnabledCount').textContent = KS.keys ? keys.filter(function(k){return k.enabled;}).length : '—';
  var query = KS.query.toLowerCase();
  var filtered = keys.filter(function(k){return (k.name + ' ' + (k.note || '')).toLowerCase().indexOf(query) >= 0;});
  if (!filtered.length){
    var message = !KS.keys ? (KS.error ? '暂时无法加载密钥' : '正在加载密钥…') : (query ? '没有匹配的密钥' : '还没有密钥');
    $('#keyRows').innerHTML = '<tr><td colspan="10">' + emptyBox(IC.box, message, !query && KS.keys ? '创建一把密钥，用于连接你的客户端。' : '') + '</td></tr>';
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
      '<td data-l="总用量" class="mono key-tokens">' + (Number(key.total_tokens) > 0 ? esc(compactTokens(key.total_tokens)) : '<span class="sub">0</span>') + '</td>' +
      '<td data-l="状态"><span class="bdg ' + (key.enabled ? 'ok' : 'off') + '"><i></i>' + (key.enabled ? '启用' : '停用') + '</span></td>' +
      '<td data-l="重复推理保护"><button type="button" class="btn sm key-guard-toggle" role="switch" aria-checked="' + keyGuardEnabled(key) +
      '" aria-label="' + esc(key.name) + '的重复推理保护" data-key-action="guard" data-id="' + esc(key.id) +
      '" title="发现持续重复输出（推理或正文）时结束该次请求；只影响此密钥后续请求">' + (keyGuardEnabled(key) ? '已开启' : '已关闭') + '</button></td>' +
      '<td data-l="CN 回落"><button type="button" class="btn sm key-guard-toggle" role="switch" aria-checked="' + keyGlobalFallbackEnabled(key) +
      '" aria-label="' + esc(key.name) + '的 CN 回落" data-key-action="fallback" data-id="' + esc(key.id) +
      '" title="global 号全部被限流时，改用同名 CN 模型继续；只影响此密钥后续请求">' + (keyGlobalFallbackEnabled(key) ? '已开启' : '已关闭') + '</button></td>' +
      '<td data-l="有效期"><span class="' + expiry.cls + '"' + (expiry.title ? ' title="' + esc(expiry.title) + '"' : '') + '>' + esc(expiry.text) + '</span></td>' +
      '<td data-l="创建时间" class="mono key-date">' + esc(fmtTime(key.created_at)) + '</td>' +
      '<td data-l="操作"><div class="key-actions"><button class="btn sm" data-key-action="edit" data-id="' + esc(key.id) + '">编辑</button>' +
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
  // 有效期默认「无限制」：既有密钥和新密钥都保持这个默认，只有显式选择才设置。
  // 编辑一把已经设过有效期的密钥时回填原值，避免保存时把有效期无声清掉。
  var preset = key && key.expires_at ? expiryToInput(key.expires_at) : '';
  $('#keyExpiry').value = preset ? 'custom' : 'none';
  $('#keyExpiryCustom').value = preset;
  $('#keyExpiryCustomField').classList.toggle('hide', !preset);
  $('#keyModelPick').value = '';
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
$('#keyModelPick').addEventListener('change', function(){
  var value = this.value; this.value = ''; if (!value) return;
  var models = keyModels($('#keyModels').value);
  if (models.indexOf(value) < 0) models.push(value);
  fillModelInput(models);
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
