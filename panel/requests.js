"use strict";
// ═══ 更新日志 ═══
// 2026-09-25：密钥列表可直接打开对应调用明细，切换调用方时作废旧分页响应。
// 2026-09-25：新增只读请求明细页，保留未知用量、真实调度与重试；隔离乱序响应并避免翻页后自动刷新。
// 2026-09-25：显示服务报告的明细写入失败次数，避免把可见记录误当成完整历史。
// 2026-09-25：显示真实调度总数与截断提示，保留记录不再冒充完整调度历史。
(function(){
  var state = {ready:false, loaded:false, loading:false, error:'', revision:0, offset:0, limit:20, total:0, items:[], filters:{}, keyOptions:Object.create(null), modelOptions:Object.create(null), facetsLoaded:false, facetsLoading:false, facetsTruncated:false, keyDisplay:Object.create(null)};
  var detail = {revision:0, active:false, requestID:'', opener:null};
  var statusNames = {success:'成功', error:'失败', canceled:'已取消', rejected:'已拒绝'};
  var protocolNames = {chat_completions:'Chat Completions', responses:'Responses', anthropic_messages:'Anthropic Messages', gemini_generate_content:'Gemini generateContent'};
  var finishNames = {stop:'正常结束',end_turn:'正常结束',stop_sequence:'命中停止条件',length:'达到输出上限',max_tokens:'达到输出上限',max_completion_tokens:'达到输出上限',content_filter:'内容受到过滤',tool_calls:'转为工具调用',function_call:'转为工具调用',pause_turn:'等待继续'};
  var reasonNames = {
    sticky_hit:'继续使用会话绑定账号', sticky_unavailable:'会话绑定账号暂不可用',
    weighted_selection:'按权重选择账号', free_preferred:'优先选择已观测免费账号',
    unknown_preferred:'选择成本待确认的账号', unknown_exploration:'探测成本待确认的账号',
    recent_lru:'选择较久未使用的账号', no_available_account:'没有可用账号',
    cooldown_fallback:'使用冷却兜底账号', weighted:'按权重选择账号', retry:'为重试重新选择账号',
    already_tried:'本次请求已尝试', realm_mismatch:'区域不匹配', disabled:'账号已停用',
    account_limit:'账号额度受限', account_cooldown:'账号冷却中', circuit_breaker:'账号熔断中',
    model_cooldown:'该模型冷却中', in_flight_full:'账号并发已满', missing_account:'绑定账号不存在',
    fallback_no_cooldown:'不符合冷却兜底条件',
    channel_rejected:'上游通道拒绝请求', upstream_not_reported:'上游没有上报用量',
    upstream_error:'上游请求失败', upstream_canceled:'上游调用已取消', client_canceled:'客户端已取消',
    incomplete_usage:'用量未完整上报', incomplete_stream:'上游响应未完整结束',
    invalid_tool_arguments:'工具参数不完整或无效', context_length_exceeded:'上下文超出限制',
    reasoning_loop_guard:'触发重复推理保护', timeout:'请求超时', rate_limited:'请求受到限流',
    unobserved:'尚无成本观测', expired:'成本观测已过期', invalid_observation:'成本观测无效', model_unspecified:'未指定模型'
  };
  var stageNames = {pool_total:'账号总数', evaluated:'已检查', healthy:'状态可用', available:'可承接请求',
    free:'已观测免费', paid:'已观测付费', unknown:'成本未知', candidates:'候选', cost_tier:'成本分组',
    selected_tier:'选中分组', recent:'近期使用', outside_recent:'近期未使用', weighted:'进入权重选择',
    cost_free:'已观测免费',cost_unknown:'成本未知',cost_paid:'已观测付费',preferred:'优先分组',shortlist:'筛后候选',eligible:'参与选择'};

  function node(id){ return document.getElementById(id); }
  function lookup(map,key,fallback){return Object.prototype.hasOwnProperty.call(map,key) ? map[key] : fallback;}
  function escapeHTML(value){ return String(value == null ? '' : value).replace(/[&<>"']/g, function(c){ return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]; }); }
  function text(id, value){ var n=node(id); if(n) n.textContent=String(value == null ? '' : value); }
  function show(id, visible){ var n=node(id); if(n) n.classList.toggle('hide', !visible); }
  function finite(value){ return typeof value === 'number' && Number.isFinite(value) && value >= 0; }
  function count(value){ return finite(value) && Number.isSafeInteger(value) ? value : 0; }
  function metric(value, credit){ return finite(value) ? value.toLocaleString('zh-CN', {maximumFractionDigits:credit ? 6 : 0}) : '未知'; }
  function duration(value){ if(!finite(value)) return '未知'; if(value < 1000) return metric(value)+' ms'; return (value/1000).toLocaleString('zh-CN', {maximumFractionDigits:2})+' s'; }
  function when(value, full){
    if(typeof value !== 'string' || value.indexOf('0001-') === 0) return '—';
    var d=new Date(value); if(!Number.isFinite(d.getTime())) return '—';
    function pad(x){return x < 10 ? '0'+x : String(x);}
    return (full ? d.getFullYear()+'-' : '')+pad(d.getMonth()+1)+'-'+pad(d.getDate())+' '+pad(d.getHours())+':'+pad(d.getMinutes())+':'+pad(d.getSeconds());
  }
  function shortID(value){ value=String(value || ''); return value.length > 28 ? value.slice(0,14)+'…'+value.slice(-8) : (value || '—'); }
  function reason(code){
    code=String(code || '');
    if(lookup(reasonNames,code,'')) return reasonNames[code];
    if(code.indexOf('fallback_')===0 && lookup(reasonNames,code.slice(9),'')) return '兜底检查：'+reasonNames[code.slice(9)];
    return code || '未记录调度原因';
  }
  function statusBadge(value, httpStatus){
    var known=Object.prototype.hasOwnProperty.call(statusNames,value), label=known ? statusNames[value] : '未知状态';
    return '<span class="requests-badge '+(known ? value : 'unknown')+'">'+escapeHTML(label)+'</span>'+(count(httpStatus) ? '<span class="requests-sub">'+escapeHTML(httpStatus)+'</span>' : '');
  }
  function usageLabel(item){
    if(item.upstream_started === false || item.usage_state === 'not_started') return '未调用上游';
    if(item.usage_state === 'complete') return '输入/输出齐全';
    return item.usage_state === 'partial' ? '部分用量' : '用量未上报';
  }
  function finishLabel(value){return lookup(finishNames,value,String(value || '未上报'));}
  function finishBadge(value){
    if(!value) return '';
    var limited=value==='length' || value==='max_tokens' || value==='max_completion_tokens' || value==='content_filter';
    return '<span class="requests-finish'+(limited ? ' limited' : '')+'" title="'+escapeHTML(value)+'">'+escapeHTML(finishLabel(value))+'</span>';
  }
  function metricCell(value, missing, started, credit){
    if(started === false) return '<span class="requests-null" title="未调用上游">—</span>';
    if(!finite(value)) return '<span class="requests-null" title="上游没有上报此项，不能按零计算">未知</span>';
    var note=count(missing) > 0 ? '<span class="requests-partial" title="'+escapeHTML(missing)+' 次尝试未上报此项，仅显示已知消耗">部分</span>' : '';
    return '<span class="requests-metric">'+escapeHTML(metric(value,credit))+'</span>'+note;
  }
  function decisionCell(d,item){
    var total=count(item && item.decision_count), truncated=!!(item && item.decisions_truncated);
    var history=total>0 || truncated ? '<span class="requests-sub">'+(total>0 ? total+' 次调度' : '调度次数未知')+(truncated ? ' · 明细已截断' : '')+'</span>' : '';
    if(!d || typeof d !== 'object') return '<span class="requests-null">未记录</span>'+history;
    var identity=String(d.account_id || '');
    if(d.account_id_truncated) identity+='…';
    return '<div class="requests-decision">'+escapeHTML(reason(d.reason_code))+'</div>'+
      (d.blocked_by ? '<span class="requests-sub">'+escapeHTML(reason(d.blocked_by))+'</span>' : '')+
      (identity ? '<span class="requests-sub mono" title="'+escapeHTML(identity)+'">'+escapeHTML(shortID(identity))+'</span>' : '')+history;
  }
  function renderRows(){
    var rows=node('requestRows'); if(!rows) return;
    if(state.loading){ rows.innerHTML='<tr><td colspan="13"><div class="empty">正在加载请求明细…</div></td></tr>'; return; }
    if(state.error){rows.innerHTML='<tr><td colspan="13"><div class="empty">暂时无法读取明细，请重试。</div></td></tr>';return;}
    if(!state.items.length){
      rows.innerHTML='<tr><td colspan="13"><div class="empty">'+(state.offset > 0 ? '这一页已没有记录，点击刷新返回最新记录。' : '没有匹配的已完成请求，可以调整筛选条件。')+'</div></td></tr>'; return;
    }
    rows.innerHTML=state.items.map(function(item){
      var usageClass=item.usage_state==='partial' ? 'partial' : (item.usage_state==='missing' ? 'missing' : '');
      var title=count(item.unknown_attempts) ? count(item.unknown_attempts)+' 次尝试未完整上报用量' : '输入与输出的上报情况；可选计量仍可能缺失';
      return '<tr data-request-id="'+escapeHTML(item.request_id)+'"><td><span class="requests-time">'+escapeHTML(when(item.started_at))+'</span><br><button type="button" class="requests-id" title="'+escapeHTML(item.request_id)+'" aria-label="查看请求 '+escapeHTML(item.request_id)+'">'+escapeHTML(shortID(item.request_id))+'</button></td>'+
        '<td><div class="requests-model">'+escapeHTML(item.model || '未确认模型')+'</div><span class="requests-sub">'+escapeHTML(lookup(protocolNames,item.protocol,'未知协议'))+' · '+(item.stream ? '流式' : '非流式')+'</span></td>'+
        '<td><div class="requests-key">'+escapeHTML(item.key_name || item.key_id || '未确认调用方')+'</div>'+(item.key_name && item.key_id ? '<span class="requests-sub mono" title="'+escapeHTML(item.key_id)+'">'+escapeHTML(shortID(item.key_id))+'</span>' : '')+'</td>'+
        '<td>'+statusBadge(item.status,item.http_status)+finishBadge(item.finish_reason)+'<span class="requests-coverage '+usageClass+'" title="'+escapeHTML(title)+'">'+escapeHTML(usageLabel(item))+'</span></td>'+
        '<td class="num">'+metricCell(item.input_tokens,item.missing_input_attempts,item.upstream_started)+'</td>'+
        '<td class="num">'+metricCell(item.output_tokens,item.missing_output_attempts,item.upstream_started)+'</td>'+
        '<td class="num">'+metricCell(item.cached_tokens,item.missing_cached_attempts,item.upstream_started)+'</td>'+
        '<td class="num">'+metricCell(item.reasoning_tokens,item.missing_reasoning_attempts,item.upstream_started)+'</td>'+
        '<td class="num">'+metricCell(item.credit,item.missing_credit_attempts,item.upstream_started,true)+'</td>'+
        '<td class="num requests-metric">'+escapeHTML(duration(item.duration_ms))+'</td><td class="num requests-metric">'+escapeHTML(duration(item.ttfb_ms))+'</td><td class="num requests-metric">'+escapeHTML(duration(item.queue_ms))+'</td><td>'+decisionCell(item.last_decision,item)+'</td></tr>';
    }).join('');
  }
  function renderPagination(){
    text('requestsRange',state.loading ? '正在加载…' : (state.error ? '暂时无法读取条数' : (state.total ? (state.items.length ? (state.offset+1)+'–'+(state.offset+state.items.length)+' / '+state.total+' 条' : '本页为空 / 共 '+state.total+' 条') : '0 条记录')));
    text('requestsPageNumber',Math.floor(state.offset/state.limit)+1);
    node('requestsPrevious').disabled=state.loading || state.offset===0;
    node('requestsNext').disabled=state.loading || !state.items.length || state.offset+state.items.length>=state.total;
    node('requestPageSize').disabled=state.loading;
    node('btnRequestsReload').disabled=state.loading;
    node('requestsPanel').setAttribute('aria-busy',state.loading ? 'true' : 'false');
  }
  // 筛选项来自服务端的 /requests/facets，而不是当前这一页的 20 条。
  // 旧实现只从本页凑：不在本页的密钥与模型永远选不到，首次加载前下拉更是空的，
  // 用户点开箭头什么都看不到（2026-09-30 反馈）。列表变化很慢，拉一次缓存即可。
  // 下拉只显示名称。<option> 的 value 决定选中后写进输入框的内容，label 会让浏览器
  // 再渲染一行，于是同一个密钥出现两行（ID + 名称）——用户只需要认名称（2026-09-30 反馈）。
  // 改为 value 直接放展示名，提交时解析回 ID；名称重复或为空时退回显示 ID，避免歧义。
  function keyDisplayName(id){
    var name=String(state.keyOptions[id]||'');
    if(!name || name===id) return id;
    var clash=Object.keys(state.keyOptions).some(function(other){ return other!==id && String(state.keyOptions[other])===name; });
    return clash ? id : name;
  }
  function renderOptions(){
    var byName={};
    node('requestKeyOptions').innerHTML=Object.keys(state.keyOptions).map(function(id){
      var shown=keyDisplayName(id);
      byName[shown]=id;
      return '<option value="'+escapeHTML(shown)+'"></option>';
    }).join('');
    state.keyDisplay=byName;
    node('requestModelOptions').innerHTML=Object.keys(state.modelOptions).map(function(model){return '<option value="'+escapeHTML(model)+'"></option>';}).join('');
  }
  // 输入框里可能是下拉选的名称，也可能是直接粘贴的管理 ID；能解析成 ID 就用 ID。
  function resolveKeyID(value){
    var trimmed=String(value||'').trim();
    if(!trimmed) return '';
    if(state.keyDisplay && Object.prototype.hasOwnProperty.call(state.keyDisplay,trimmed)) return state.keyDisplay[trimmed];
    // 兜底：按名称在已知密钥里找唯一匹配（例如从「查看该密钥的明细」跳进来时还没渲染过下拉）。
    var matches=Object.keys(state.keyOptions).filter(function(id){return String(state.keyOptions[id])===trimmed;});
    return matches.length===1 ? matches[0] : trimmed;
  }
  async function loadFacets(){
    if(state.facetsLoaded || state.facetsLoading) return;
    state.facetsLoading=true;
    try{
      var result=await api('api/requests/facets');
      if(result && result.ok===true){
        (result.keys||[]).forEach(function(item){
          if(item && typeof item.id==='string' && item.id) state.keyOptions[item.id]=String(item.name||item.id);
        });
        (result.models||[]).forEach(function(model){
          if(typeof model==='string' && model) state.modelOptions[model]=true;
        });
        state.facetsLoaded=true;
        state.facetsTruncated=result.truncated===true;
        renderOptions();
      }
    }catch(error){
      // 拉不到就退回「从当前页凑」，至少不阻断主流程。
      collectFromPage();
    }finally{
      state.facetsLoading=false;
    }
  }
  // 兜底：服务端筛选项不可用时，仍用当前页已有的记录填下拉。
  function collectFromPage(){
    state.items.forEach(function(item){
      if(item.key_id && Object.keys(state.keyOptions).length < 300) state.keyOptions[item.key_id]=String(item.key_name || item.key_id);
      if(item.model && Object.keys(state.modelOptions).length < 300) state.modelOptions[item.model]=true;
    });
    renderOptions();
  }
  function updateOptions(){ collectFromPage(); }
  function errorMessage(error){ return error && error.message ? String(error.message).slice(0,240) : '加载失败，请重试'; }
  function readFilters(){
    return {key_id:resolveKeyID(node('requestKeyFilter').value),model:node('requestModelFilter').value.trim(),status:node('requestStatusFilter').value,request_id:node('requestIDFilter').value.trim()};
  }
  async function fetchPage(){
    var revision=++state.revision, offset=state.offset, limit=state.limit, filters=Object.assign({},state.filters);
    var query=new URLSearchParams();
    Object.keys(filters).forEach(function(key){if(filters[key]) query.set(key,filters[key]);});
    query.set('offset',String(offset));query.set('limit',String(limit));
    state.loading=true;state.error='';state.items=[];show('requestsError',false);show('requestsRecovery',false);
    text('requestsStatus','正在加载…');renderRows();renderPagination();
    try{
      // Relative paths preserve both the original panel root and /admin/ in the
      // unified container. An absolute /api path would escape the panel mount.
      var result=await api('api/requests?'+query.toString());
      if(revision!==state.revision) return;
      if(!result || result.ok!==true) throw new Error((result && result.message) || '请求明细暂不可用');
      if(!Array.isArray(result.items) || result.items.length>100 || !Number.isSafeInteger(result.total) || result.total<0 || !Number.isSafeInteger(result.offset) || result.offset<0 || !Number.isSafeInteger(result.limit) || result.limit<1 || result.limit>100 || result.items.some(function(item){return !item || typeof item.request_id!=='string';})) throw new Error('服务返回的请求明细不完整');
      state.items=result.items;state.total=result.total;state.offset=result.offset;state.limit=result.limit;state.loaded=true;
      updateOptions();
      text('requestsStatus','已更新 · '+when(new Date().toISOString()).split(' ')[1]);
      var warnings=[];
      if(count(result.recording_errors)>0) warnings.push('服务报告 '+count(result.recording_errors)+' 次明细写入失败，当前列表可能不完整。');
      if(result.recovery && count(result.recovery.count)>0){
        warnings.push('日志曾恢复 '+count(result.recovery.count)+' 次损坏尾记录，已保留此前完整记录。最近恢复：'+when(result.recovery.last_recovered_at,true));
      }
      if(warnings.length){text('requestsRecovery',warnings.join(' '));show('requestsRecovery',true);}
    }catch(error){
      if(revision!==state.revision) return;
      state.loaded=false;state.items=[];state.total=0;state.error=errorMessage(error);text('requestsError',state.error);show('requestsError',true);text('requestsStatus','加载失败');
    }finally{
      if(revision===state.revision){state.loading=false;renderRows();renderPagination();}
    }
  }
  function meta(label,value,wide){return '<div'+(wide ? ' class="wide"' : '')+'><dt>'+escapeHTML(label)+'</dt><dd>'+escapeHTML(value == null || value==='' ? '—' : value)+'</dd></div>';}
  function renderCounts(counts,fallback){
    if(!counts || typeof counts!=='object') return '';
    return Object.keys(counts).slice(0,32).filter(function(key){return (key.indexOf('fallback_')===0)===fallback;}).map(function(key){
      var label=lookup(stageNames,key,'') || lookup(stageNames,key.replace(/^fallback_/,''),'') || reason(key);
      return '<span title="'+escapeHTML(key)+'">'+escapeHTML(label)+' '+escapeHTML(metric(counts[key]))+'</span>';
    }).join('');
  }
  function renderDecision(d){
    var normal=renderCounts(d.excluded_counts,false), fallback=renderCounts(d.excluded_counts,true);
    var selected=d.account_id ? '选中账号 '+d.account_id+(d.account_id_truncated ? '（标识已截短）' : '') : '';
    var output='<div class="requests-decision-detail"><h5>'+escapeHTML(reason(d.reason_code))+'</h5><div class="requests-code">'+escapeHTML(d.reason_code || '未记录代码')+'</div>';
    if(d.observed_at) output+='<p>调度时间：'+escapeHTML(when(d.observed_at,true))+'</p>';
    if(selected) output+='<p>'+escapeHTML(selected)+(d.account_realm ? ' · '+escapeHTML(d.account_realm) : '')+'</p>';
    if(d.bound_account_id) output+='<p>会话绑定账号：'+escapeHTML(d.bound_account_id)+'</p>';
    if(d.blocked_by) output+='<p>'+escapeHTML(reason(d.blocked_by))+' <span class="requests-code">'+escapeHTML(d.blocked_by)+'</span></p>';
    var stages=renderCounts(d.stage_counts,false), fallbackStages=renderCounts(d.stage_counts,true);
    if(stages) output+='<div class="requests-counts">'+stages+'</div>';
    if(normal) output+='<p>常规筛选排除</p><div class="requests-counts">'+normal+'</div>';
    if(fallback || fallbackStages) output+='<p>兜底检查（单独统计）</p><div class="requests-counts">'+fallbackStages+fallback+'</div>';
    if(d.selection_method==='weighted' && finite(d.weight_units) && finite(d.weight_total) && d.weight_total>0){
      output+='<p>本次抽签权重：'+escapeHTML(metric(d.weight_units))+' / '+escapeHTML(metric(d.weight_total))+'</p>';
    }
    if(d.cost_state==='free' || d.cost_state==='paid'){
      output+='<p>历史成本观测：'+escapeHTML(metric(d.cost_per_1k,true))+' 积分 / 千 tokens'+(d.cost_used_for_selection ? '，本次用于选择。' : '，本次未用于选择。')+'这不是本次消费。</p>';
      if(d.cost_observed_at) output+='<p>观测时间：'+escapeHTML(when(d.cost_observed_at,true))+' · '+escapeHTML(metric(d.cost_samples))+' 次样本</p>';
    }else if(d.cost_state==='unknown') output+='<p>历史成本未知'+(d.cost_unknown_reason ? '：'+escapeHTML(reason(d.cost_unknown_reason)) : '')+'</p>';
    if(d.fallback_kind || d.fallback_until) output+='<p>兜底类型：'+escapeHTML(d.fallback_kind || '未记录')+'；原冷却截止：'+escapeHTML(when(d.fallback_until,true))+'</p>';
    return output+'</div>';
  }
  function renderAttempt(a,decisions){
    var result='<li class="requests-attempt"><div class="requests-attempt-head"><b>第 '+escapeHTML(a.number)+' 次尝试</b>'+statusBadge(a.status,a.http_status)+'<span class="sp"></span><span class="requests-sub">'+escapeHTML(duration(a.duration_ms))+'</span></div>';
    var account=a.account_name && a.account_id ? a.account_name+' · '+a.account_id : (a.account_name || a.account_id);
    result+='<dl class="requests-detail-meta">'+meta('账号',account)+meta('开始时间',when(a.started_at,true))+meta('结束时间',when(a.finished_at,true))+meta('排队',duration(a.queue_ms))+meta('完整模型',a.model,true)+'</dl>';
    result+='<div class="requests-attempt-metrics">'+[['输入',a.input_tokens],['输出',a.output_tokens],['缓存',a.cached_tokens],['推理',a.reasoning_tokens],['积分',a.credit,true]].map(function(m){return '<div><span>'+m[0]+'</span><b>'+escapeHTML(metric(m[1],m[2]))+'</b></div>';}).join('')+'</div>';
    var usage=a.usage_state==='complete' ? '输入、输出已上报；可选计量仍可能缺失。' : (a.usage_state==='partial' ? '只收到了部分计量，未知部分没有按零补齐。' : '上游未上报用量，不能判断本次实际消耗。');
    result+='<p class="requests-attempt-explanation">'+usage+'</p>';
    if(a.finish_reason) result+='<p class="requests-attempt-explanation">结束原因：'+escapeHTML(finishLabel(a.finish_reason))+' <span class="requests-code">'+escapeHTML(a.finish_reason)+'</span></p>';
    if(a.usage_reason) result+='<p class="requests-attempt-explanation">'+escapeHTML(reason(a.usage_reason))+' <span class="requests-code">'+escapeHTML(a.usage_reason)+'</span></p>';
    if(a.error_code) result+='<p class="requests-attempt-explanation">'+escapeHTML(reason(a.error_code))+' <span class="requests-code">'+escapeHTML(a.error_code)+'</span></p>';
    decisions.forEach(function(d){result+=renderDecision(d);});
    return result+'</li>';
  }
  function renderDetail(record){
    var attempts=Array.isArray(record.attempts) ? record.attempts : [], decisions=Array.isArray(record.decisions) ? record.decisions : [];
    if(attempts.length>64 || decisions.length>64) throw new Error('请求详情超过显示上限');
    var decisionTotal=count(record.decision_count) || decisions.length;
    var decisionsTruncated=record.decisions_truncated===true || decisionTotal>decisions.length;
    var output='<dl class="requests-detail-meta">'+meta('完整模型',record.model,true)+meta('调用密钥',record.key_name || record.key_id)+meta('密钥管理 ID',record.key_id)+meta('协议',lookup(protocolNames,record.protocol,'未知协议'))+meta('开始时间',when(record.started_at,true))+meta('完成时间',when(record.finished_at,true))+meta('状态',lookup(statusNames,record.status,'未知状态')+(count(record.http_status) ? ' · '+record.http_status : ''))+meta('总耗时',duration(record.duration_ms))+meta('首字节',duration(record.ttfb_ms))+meta('排队',duration(record.queue_ms))+'</dl>';
    if(record.error_code) output+='<p class="requests-detail-note">'+escapeHTML(reason(record.error_code))+' <span class="requests-code">'+escapeHTML(record.error_code)+'</span></p>';
    if(record.finish_reason) output+='<p class="requests-detail-note">结束原因：'+escapeHTML(finishLabel(record.finish_reason))+' <span class="requests-code">'+escapeHTML(record.finish_reason)+'</span></p>';
    if(record.upstream_started===false){output+='<p class="requests-detail-note">请求未调用上游，没有上游消费观测。</p>';}
    else{
      output+='<p class="requests-detail-note">共 '+escapeHTML(metric(record.attempt_count))+' 次上游尝试；每次的已知消耗独立保留，未知用量无法补算。</p>';
      if(record.attempts_truncated) output+='<p class="requests-detail-note warn">保留了 '+attempts.length+' 次尝试明细，其余 '+Math.max(0,count(record.attempt_count)-attempts.length)+' 次仍计为未知。此处不是完整消费账单。</p>';
    }
    if(decisionsTruncated) output+='<p class="requests-detail-note warn">共 '+decisionTotal+' 次调度，保留 '+decisions.length+' 条记录；调度历史已截断。</p>';
    else if(decisionTotal>0) output+='<p class="requests-detail-note">共 '+decisionTotal+' 次调度。</p>';
    output+='<h4 class="requests-detail-title">尝试时间线</h4>';
    if(attempts.length){
      output+='<ol class="requests-timeline">'+attempts.map(function(a){return renderAttempt(a,decisions.filter(function(d){return d && d.attempt===a.number;}));}).join('')+'</ol>';
    }else output+='<p class="requests-detail-note">没有上游尝试明细。</p>';
    var unmatched=decisions.filter(function(d){return d && !attempts.some(function(a){return a.number===d.attempt;});});
    if(unmatched.length) output+='<h4 class="requests-detail-title">其他调度记录</h4>'+unmatched.map(renderDecision).join('');
    node('requestDetailBody').innerHTML=output;
  }
  function closeDetail(){
    detail.revision++;detail.active=false;detail.requestID='';
    var dialog=node('requestDetailDialog');
    if(dialog && dialog.open && typeof dialog.close==='function') dialog.close();
    else if(dialog) dialog.removeAttribute('open');
    text('requestDetailID','');node('requestDetailBody').innerHTML='';show('requestDetailRetry',false);
    if(detail.opener && detail.opener.isConnected!==false && typeof detail.opener.focus==='function') detail.opener.focus();
    detail.opener=null;
  }
  async function openDetail(requestID){
    var revision=++detail.revision;
    if(!detail.active) detail.opener=document.activeElement;
    detail.active=true;detail.requestID=requestID;text('requestDetailID',requestID);show('requestDetailRetry',false);
    node('requestDetailBody').innerHTML='<div class="empty" role="status">正在加载请求详情…</div>';
    var dialog=node('requestDetailDialog');
    if(!dialog.open){if(typeof dialog.showModal==='function') dialog.showModal();else dialog.setAttribute('open','');}
    try{
      var result=await api('api/requests/'+encodeURIComponent(requestID));
      if(revision!==detail.revision || !detail.active) return;
      if(!result || result.ok!==true || !result.record || result.record.request_id!==requestID) throw new Error((result && result.message) || '请求详情不可用或已经过期');
      renderDetail(result.record);
    }catch(error){
      if(revision!==detail.revision || !detail.active) return;
      node('requestDetailBody').innerHTML='<div class="note err" role="alert">'+escapeHTML(errorMessage(error))+'</div>';show('requestDetailRetry',true);
    }
  }
  function init(){
    if(state.ready) return true;
    if(!node('pg-requests')) return false;
    state.ready=true;
    node('requestsRecovery').classList.add('w');node('requestsError').classList.add('e');
    node('requestFilters').addEventListener('submit',function(event){event.preventDefault();state.filters=readFilters();state.offset=0;return fetchPage();});
    node('requestFilters').addEventListener('reset',function(event){
      event.preventDefault();['requestKeyFilter','requestModelFilter','requestStatusFilter','requestIDFilter'].forEach(function(id){node(id).value='';});
      state.filters={};state.offset=0;return fetchPage();
    });
    node('btnRequestsReload').addEventListener('click',function(){state.offset=0;return fetchPage();});
    // 进页面就拉筛选项，用户点箭头时下拉已经有内容。
    loadFacets();
    node('requestPageSize').addEventListener('change',function(){state.limit=node('requestPageSize').value==='50' ? 50 : 20;state.offset=0;return fetchPage();});
    node('requestsPrevious').addEventListener('click',function(){if(state.loading || state.offset===0) return;state.offset=Math.max(0,state.offset-state.limit);return fetchPage();});
    node('requestsNext').addEventListener('click',function(){if(state.loading || !state.items.length || state.offset+state.items.length>=state.total) return;state.offset+=state.limit;return fetchPage();});
    node('requestRows').addEventListener('click',function(event){
      var row=event.target && typeof event.target.closest==='function' ? event.target.closest('tr[data-request-id]') : null;
      if(!row || state.loading) return;
      if(typeof window.getSelection==='function' && String(window.getSelection())) return;
      return openDetail(row.getAttribute('data-request-id'));
    });
    node('requestDetailClose').addEventListener('click',closeDetail);node('requestDetailDone').addEventListener('click',closeDetail);
    node('requestDetailDialog').addEventListener('cancel',function(event){event.preventDefault();closeDetail();});
    node('requestDetailDialog').addEventListener('close',function(){if(detail.active) closeDetail();});
    node('requestDetailRetry').addEventListener('click',function(){if(detail.active) return openDetail(detail.requestID);});
    if(typeof window.addEventListener==='function') window.addEventListener('hashchange',function(){if(detail.active && location.hash!=='#requests') closeDetail();});
    return true;
  }
  // The navigation/global refresh can call this without moving an older page.
  // Explicit refresh passes true; the page's refresh button always starts at 0.
  window.loadRequests=function(force){
    if(!init()) return Promise.resolve();
    if(force===true){state.offset=0;return fetchPage();}
    if(state.loading || (state.loaded && state.offset>0)) return Promise.resolve();
    return fetchPage();
  };
  window.openRequestsForKey=function(keyID,keyName){
    if(!init() || typeof keyID!=='string' || !keyID) return Promise.resolve();
    closeDetail();
    state.keyOptions[keyID]=String(keyName || keyID);
    node('requestKeyFilter').value=keyDisplayName(keyID);
    ['requestModelFilter','requestStatusFilter','requestIDFilter'].forEach(function(id){node(id).value='';});
    state.filters=readFilters();state.offset=0;state.loaded=false;
    // Start the selected-key load before navigation so go() cannot launch an
    // unfiltered request or replace it with an older response already in flight.
    var pending=fetchPage();
    updateOptions();
    if(typeof go==='function') go('requests');
    return pending;
  };
  if(document.readyState==='loading') document.addEventListener('DOMContentLoaded',init);else init();
})();
