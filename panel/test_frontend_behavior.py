#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-24：运行真实前端函数复现零值时间、刷新乱序、日志内容变化和筛选用量口径，防止字符串检查漏掉行为回退。

import json
import os
from pathlib import Path
import shutil
import subprocess
import unittest


PANEL = Path(__file__).resolve().parent
NODE = shutil.which("node")


@unittest.skipUnless(NODE, "Node.js is required for frontend behavior checks")
class FrontendBehaviorTests(unittest.TestCase):
    def run_frontend(self, expression, extra_sources=()):
        program = r"""
const vm = require('vm'), fs = require('fs'), path = require('path');
const nodes = new Map();
// 归一化：document.querySelector('#x') 与 getElementById('x') 必须指向同一个节点，
// 否则同一元素会被存成两个 key——模块用 getElementById 绑定、测试用 $() 读取，
// 两边看到的是不同对象，断言必然假失败。
function norm(id) {
  const s = String(id);
  return s.charAt(0) === '#' ? s.slice(1) : s;
}
function node(rawId) {
  const id = norm(rawId);
  if (!nodes.has(id)) nodes.set(id, {innerHTML:'', textContent:'', value:'', disabled:false,
    options:[], style:{}, attrs:{}, events:{}, classList:{add(){},remove(){},toggle(){}},
    setAttribute(k,v){this.attrs[k]=String(v)}, getAttribute(k){return this.attrs[k]},
    removeAttribute(k){delete this.attrs[k]}, addEventListener(k,v){this.events[k]=v},
    querySelector(s){return node(id+' '+s)},querySelectorAll(){return []}});
  return nodes.get(id);
}
// window 必须存在：requests.js 等模块在加载时就会写 window.loadRequests /
// window.openRequestsForKey，缺了它整个脚本会在解析期抛错，测试根本进不到断言。
const win = {addEventListener(){}, location:{origin:'http://fixture.invalid',hash:'#overview'}};
const context = {console, Date, Promise, Map, window:win, URLSearchParams, JSON, Object, Array, String, Number, Boolean, Error, RegExp, Math,
  setTimeout(){return 1},clearTimeout(){},setInterval(){return 1},clearInterval(){},
  location:{origin:'http://fixture.invalid',hash:'#overview',replace(){}},
  history:{replaceState(){}},localStorage:{getItem(){return null},setItem(){}},
  document:{readyState:'loading',hidden:false,documentElement:node('html'),
    // getElementById 必须存在：requests.js 的 node() 走的就是它，缺了会让
    // 整个模块在 init() 里抛错，测试永远进不到断言（此前 requests.js 从未被测过）。
    getElementById:node, querySelector:node, querySelectorAll(){return []}, addEventListener(){}}};
vm.createContext(context);
for (const source of ['app.js','usage.js'].concat(String(process.argv[3]||'').split(',').filter(Boolean)))
  vm.runInContext(fs.readFileSync(path.join(process.argv[1],source),'utf8'),context);
Promise.resolve(vm.runInContext('(async()=>{'+process.argv[2]+'})()',context))
  .then(result=>console.log(JSON.stringify(result)))
  .catch(error=>{console.error(error);process.exitCode=1});
"""
        env = dict(os.environ, TZ="Asia/Shanghai")
        result = subprocess.run([NODE, "-e", program, str(PANEL), expression, ",".join(extra_sources)],
                                capture_output=True, text=True, encoding="utf-8", env=env, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def test_zero_success_timestamp_does_not_hide_error_only_activity(self):
        html = self.run_frontend("""
S.data={accounts:[{uid:'fixture',pool:{inPool:true,cooling:true,coolRemaining:30,
lastSuccess:'0001-01-01T00:00:00Z',lastErr:'2026-09-24T06:00:00Z'}}]};
renderAccounts(); return $('#acctRows').innerHTML;
""")
        self.assertIn("无成功记录", html)
        self.assertIn("最近错误", html)
        self.assertNotIn("从未", html)

    def test_account_with_no_timestamps_is_not_described_as_having_succeeded(self):
        html = self.run_frontend("""
S.data={accounts:[{uid:'fixture',pool:{inPool:true,healthy:true,
lastSuccess:'0001-01-01T00:00:00Z',lastErr:'0001-01-01T00:00:00Z'}}]};
renderAccounts(); return $('#acctRows').innerHTML;
""")
        self.assertIn("无记录", html)
        self.assertNotIn("从未", html)

    def test_credential_valid_for_another_hour_is_not_expired(self):
        html = self.run_frontend("""
S.data={accounts:[{uid:'fixture',expiresAt:Date.now()/1000+3600,
pool:{inPool:true,healthy:true}}]};
renderAccounts(); return $('#acctRows').innerHTML;
""")
        self.assertNotIn("凭证已过期", html)
        self.assertIn("凭证剩", html)

    def test_rerendering_account_rows_does_not_restart_cooldown(self):
        result = self.run_frontend("""
var clock=1000000;Date.now=function(){return clock};S.loadedAt=clock;
S.data={accounts:[{uid:'fixture',pool:{inPool:true,cooling:true,coolRemaining:120}}]};
renderAccounts();var first=$('#acctRows').innerHTML;
clock+=60000;renderAccounts();var second=$('#acctRows').innerHTML;
return {first:first.match(/data-cool-end="(\\d+)"/)[1],
second:second.match(/data-cool-end="(\\d+)"/)[1],html:second};
""")
        self.assertEqual(result["first"], result["second"])
        self.assertIn("剩 1:00", result["html"])

    def test_expired_cooldown_does_not_repeatedly_reload_the_same_snapshot(self):
        result = self.run_frontend("""
var clock=1000000;Date.now=function(){return clock};
var entry=$('#countdown');entry.setAttribute('data-cool-end',clock-1000);
document.querySelectorAll=function(){return [entry]};
var callbacks=[],calls=0;setTimeout=function(cb){callbacks.push(cb);return 1};
loadAll=async function(){calls++};
tickCooldowns();await callbacks[0]();tickCooldowns();
return {scheduled:callbacks.length,loads:calls};
""")
        self.assertEqual(result, {"scheduled": 1, "loads": 1})

    def test_latest_state_load_wins_when_responses_arrive_out_of_order(self):
        result = self.run_frontend("""
var requests=[], seen=[];
api=function(path){return path.indexOf('api/state')===0
  ? new Promise(resolve=>requests.push(resolve)) : Promise.resolve({tasks:[]});};
render=function(){seen.push(S.data.revision)};svc=function(){};scheduleCreditRetry=function(){};
var older=loadAll(), newer=loadAll();
requests[1]({revision:2});await newer;requests[0]({revision:1});await older;
return {revision:S.data.revision,seen:seen,busy:busyDepth};
""")
        self.assertEqual(result, {"revision": 2, "seen": [2], "busy": 0})

    def test_stale_failed_state_load_does_not_hide_latest_success(self):
        result = self.run_frontend("""
var requests=[], status=[], errors=[];
api=function(path){return path.indexOf('api/state')===0
  ? new Promise((resolve,reject)=>requests.push({resolve,reject})) : Promise.resolve({tasks:[]});};
render=function(){};svc=function(up){status.push(up)};scheduleCreditRetry=function(){};
toast=function(message){errors.push(message)};
var older=loadAll(), newer=loadAll();
requests[1].resolve({revision:2});await newer;
requests[0].reject(new Error('stale request failed'));await older;
return {status:status,errors:errors,busy:busyDepth};
""")
        self.assertEqual(result, {"status": [True], "errors": [], "busy": 0})

    def test_logs_refresh_when_same_sequence_status_and_duration_have_new_content(self):
        html = self.run_frontend("""
var calls=0;api=async function(){return {ok:true,rows:[{seq:1,status:'200',total:'1.0s',
time:'12:00:00',model:++calls===1?'global:old-model':'global:new-model',
in:calls===1?'100':'500',hit:'0',tok:'5',rate:'5',mode:'stream'}]};};
await loadLogs();await loadLogs();return $('#logRows').innerHTML;
""")
        self.assertIn("global:new-model", html)
        self.assertNotIn("global:old-model", html)

    def test_changing_log_limit_during_fetch_fetches_the_requested_limit(self):
        result = self.run_frontend("""
var requested=[], release;
api=function(path){requested.push(path);return requested.length===1
  ? new Promise(resolve=>{release=resolve})
  : Promise.resolve({ok:true,rows:[{seq:2,status:'200',total:'2s',model:'new-limit'}]});};
CURRENT_VIEW='logs';S.logLines=120;var pending=loadLogs();
S.logLines=500;await loadLogs();release({ok:true,rows:[{seq:1,status:'200',total:'1s',model:'old-limit'}]});
await pending;return {requested:requested,html:$('#logRows').innerHTML};
""")
        self.assertEqual(result["requested"], ["api/logs?lines=120", "api/logs?lines=500"])
        self.assertIn("new-limit", result["html"])
        self.assertNotIn("old-limit", result["html"])

    def test_usage_bars_use_selected_period_as_denominator(self):
        html = self.run_frontend("""
US.from=US.to='2026-09-23';renderUsageRows([
{key_id:'a',name:'A',totals:{requests:100,total_tokens:100000},days:[{day:US.from,totals:{requests:1,total_tokens:10}}]},
{key_id:'b',name:'B',totals:{requests:10,total_tokens:100},days:[{day:US.from,totals:{requests:1,total_tokens:100}}]}]);
return $('#usageRows').innerHTML;
""")
        self.assertIn('width:100%', html)
        self.assertIn('width:10%', html)

    def test_usage_empty_period_does_not_list_unrelated_lifetime_rows(self):
        html = self.run_frontend("""
US.from=US.to='2026-09-23';renderUsageRows([
{key_id:'old',name:'old lifetime row',totals:{requests:20,total_tokens:200},
days:[{day:'2026-09-22',totals:{requests:20,total_tokens:200}}]}]);
return $('#usageRows').innerHTML;
""")
        self.assertIn("这个时间范围没有用量记录", html)
        self.assertNotIn("old lifetime row", html)

    def test_historical_usage_does_not_show_future_last_use_as_in_period(self):
        result = self.run_frontend("""
US.from=US.to='2026-09-23';return usageLastUsedFor({
last_used_at:'2026-09-24T01:00:00Z',days:[{day:'2026-09-23',totals:{requests:5}}]});
""")
        self.assertEqual(result, "")

    def test_last_use_within_selected_range_remains_visible(self):
        result = self.run_frontend("""
US.from=US.to='2026-09-23';return usageLastUsedFor({
last_used_at:'2026-09-23T01:00:00Z',days:[{day:'2026-09-23',totals:{requests:5}}]});
""")
        self.assertEqual(result, "2026-09-23T01:00:00Z")

    def test_global_refresh_reloads_the_visible_usage_and_log_pages(self):
        result = self.run_frontend("""
var calls=[];loadAll=function(){calls.push('state')};loadUsage=function(){calls.push('usage')};
loadLogs=function(){calls.push('logs')};
CURRENT_VIEW='usage';location.hash='#usage';$('#btnRefresh').events.click();
CURRENT_VIEW='logs';location.hash='#logs';$('#btnRefresh').events.click();return calls;
""")
        self.assertEqual(result, ["state", "usage", "state", "logs"])


    def test_update_page_reports_a_disabled_gateway(self):
        result = self.run_frontend("""
UP.data={ok:true,status:{enabled:false,current:'v2.4.10',commit:'111b07023e0a',state:'idle'}};
renderUpdate();
return {apply:$('#btnUpdApply').disabled, check:$('#btnUpdCheck').disabled,
        state:$('#updState').textContent, hint:$('#updHint').textContent,
        body:$('#updRows').innerHTML};
""", extra_sources=("update.js",))
        self.assertTrue(result["apply"], "关闭热更新后「立即更新」按钮必须禁用")
        self.assertTrue(result["check"], "关闭热更新后「检查更新」按钮必须禁用")
        self.assertEqual(result["state"], "已关闭")
        self.assertIn("update.enabled", result["body"])
        self.assertIn("手工部署", result["body"])

    def test_update_page_keeps_the_normal_ui_when_enabled(self):
        result = self.run_frontend("""
UP.data={ok:true,status:{enabled:true,current:'v2.4.10',state:'idle',latest_tag:'v2.4.11',update_ready:true}};
renderUpdate();
return {apply:$('#btnUpdApply').disabled, hint:$('#updHint').textContent};
""", extra_sources=("update.js",))
        self.assertFalse(result["apply"])
        self.assertIn("v2.4.11", result["hint"])

    def test_update_apply_disabled_when_already_latest(self):
        """已是最新版本时「立即更新」必须禁用。

        2026-09-28 实测 bug：面板只看 busy，没看 update_ready，于是显示「已是最新版本」
        的同时按钮仍可点——点下去会白下载、白验签、白交接一次，把服务重启一遍而版本号不变。
        """
        result = self.run_frontend("""
UP.data={ok:true,status:{enabled:true,visibility:'public',current:'v2.4.23',
  latest_tag:'v2.4.23',update_ready:false,checked_at:'2026-09-28T14:36:17Z',state:'idle'}};
renderUpdate();
return {apply:$('#btnUpdApply').disabled, check:$('#btnUpdCheck').disabled,
        hint:$('#updHint').textContent};
""", extra_sources=("update.js",))
        self.assertTrue(result["apply"], "已是最新版本时「立即更新」必须禁用")
        self.assertFalse(result["check"], "已是最新版本时「检查更新」仍应可用")
        self.assertIn("已是最新版本", result["hint"])

    def test_update_apply_enabled_when_an_update_exists(self):
        result = self.run_frontend("""
UP.data={ok:true,status:{enabled:true,visibility:'public',current:'v2.4.22',
  latest_tag:'v2.4.23',update_ready:true,checked_at:'2026-09-28T14:36:17Z',state:'idle'}};
renderUpdate();
return {apply:$('#btnUpdApply').disabled, hint:$('#updHint').textContent};
""", extra_sources=("update.js",))
        self.assertFalse(result["apply"], "有可升级版本时「立即更新」必须可用")
        self.assertIn("可升级到", result["hint"])

    def test_update_apply_enabled_before_first_check(self):
        """还没检查过远端时允许点「立即更新」：网关会先查一次远端版本。"""
        result = self.run_frontend("""
UP.data={ok:true,status:{enabled:true,visibility:'public',current:'v2.4.22',state:'idle'}};
renderUpdate();
return {apply:$('#btnUpdApply').disabled, hint:$('#updHint').textContent};
""", extra_sources=("update.js",))
        self.assertFalse(result["apply"], "未检查过远端时应允许点，由网关先查一次")
        self.assertIn("检查远端版本", result["hint"])

    def test_update_page_reports_private_repo_without_token(self):
        """私有仓库未配令牌：不可用，且必须说明原因（而不是一个无法解释的失败）。"""
        result = self.run_frontend("""
UP.data={ok:true,status:{enabled:true,visibility:'private',current:'v2.4.23',
  unavailable_reason:'发布仓库已转为私有，匿名读不到 Release；把仓库改回公开，或配置 update.token'}};
renderUpdate();
return {apply:$('#btnUpdApply').disabled, state:$('#updState').textContent,
        hint:$('#updHint').textContent, body:$('#updRows').innerHTML};
""", extra_sources=("update.js",))
        self.assertTrue(result["apply"], "私有仓库未配令牌时「立即更新」必须禁用")
        self.assertEqual(result["state"], "不可用")

    def test_requests_filters_are_wired_and_sent(self):
        """筛选控件必须真的绑上并发出请求。

        2026-09-30 用户反馈「筛选不行、下拉箭头没用」：服务端筛选已实测正常，
        所以先锁定前端契约——提交表单要读取四个控件并把它们发进查询串。
        """
        result = self.run_frontend("""
var sent=[];
api=function(url){sent.push(url);return Promise.resolve({ok:true,items:[],total:0,offset:0,limit:20});};
// 框架里 document.addEventListener 是空实现，DOMContentLoaded 不会触发，
// 所以显式走一次 loadRequests 让 init() 绑定事件（真实页面由 go() 调用）。
window.loadRequests();
$('#requestKeyFilter').value='legacy';
$('#requestModelFilter').value='global:deepseek-v4.1-flash';
$('#requestStatusFilter').value='success';
$('#requestIDFilter').value='req_ABC';
var handler=$('#requestFilters').events.submit;
if(!handler) return {bound:false};
handler({preventDefault:function(){}});
// fetchPage 是异步的：框架的 setTimeout 是空实现，所以用微任务等它把请求发出去。
return Promise.resolve().then(function(){}).then(function(){}).then(function(){
  return {bound:true, url:sent[sent.length-1]||''};
});
""", extra_sources=("requests.js",))
        self.assertTrue(result["bound"], "requestFilters 必须绑定 submit 处理")
        url = result["url"]
        self.assertIn("key_id=legacy", url, "调用密钥筛选未进入查询串")
        self.assertIn("status=success", url, "请求状态筛选未进入查询串")
        self.assertIn("request_id=req_ABC", url, "请求 ID 筛选未进入查询串")

    def test_request_facets_populate_the_datalists(self):
        """筛选项必须来自服务端 facets，而不是当前页那 20 条。

        2026-09-30 用户反馈「箭头点开什么都没有」：旧实现只从当前页收集，
        不在本页的密钥/模型永远选不到，首次加载前下拉是空的。
        """
        result = self.run_frontend("""
var asked=[];
api=function(url){
  asked.push(url);
  if(String(url).indexOf('facets')>=0)
    return Promise.resolve({ok:true,keys:[{id:'legacy',name:'dddmiku'},{id:'key_9',name:'热情'}],
                            models:['global:deepseek-v4.1-flash','cn:hy3'],truncated:false});
  return Promise.resolve({ok:true,items:[],total:0,offset:0,limit:20});
};
window.loadRequests();
return Promise.resolve().then(function(){}).then(function(){}).then(function(){
  // 桩节点不会把 innerHTML 解析成 options，所以直接断言写入的内容——
  // 真实浏览器里的解析已单独用无头 Chrome 验证过（2/2 选项）。
  return {asked:asked, keys:$('#requestKeyOptions').innerHTML, models:$('#requestModelOptions').innerHTML};
});
""", extra_sources=("requests.js",))
        self.assertTrue(any('facets' in u for u in result["asked"]), "必须请求 /requests/facets")
        # 下拉只显示名称：value 是展示名，不再同时带 label（否则同一密钥显示两行）。
        self.assertIn('value="dddmiku"', result["keys"], "密钥下拉应显示名称")
        self.assertNotIn('label=', result["keys"], "不应再用 label 渲染第二行")
        self.assertNotIn('value="legacy"', result["keys"], "不应把管理 ID 当显示项")
        self.assertIn('global:deepseek-v4.1-flash', result["models"], "模型下拉应含完整模型名")

    def test_request_key_filter_resolves_name_back_to_id(self):
        """下拉显示名称，但提交必须换回管理 ID——否则筛选会查不到任何记录。"""
        result = self.run_frontend("""
var sent=[];
api=function(url){
  if(String(url).indexOf('facets')>=0)
    return Promise.resolve({ok:true,keys:[{id:'legacy',name:'dddmiku'}],models:['m'],truncated:false});
  sent.push(url);
  return Promise.resolve({ok:true,items:[],total:0,offset:0,limit:20});
};
window.loadRequests();
var wait=Promise.resolve();
for(var i=0;i<8;i++) wait=wait.then(function(){});
return wait.then(function(){
  $('#requestKeyFilter').value='dddmiku';
  $('#requestFilters').events.submit({preventDefault:function(){}});
  var w2=Promise.resolve();
  for(var j=0;j<8;j++) w2=w2.then(function(){});
  return w2.then(function(){ return sent; });
});
""", extra_sources=("requests.js",))
        # facets 桩数据里 dddmiku 对应 legacy
        joined = " ".join(result)
        self.assertIn("key_id=legacy", joined, "名称应被解析回管理 ID")
        self.assertNotIn("key_id=dddmiku", joined, "不应把名称直接当 ID 发出")

    def test_overview_credit_breaks_down_by_realm(self):
        """首页积分要分列国际与国内。

        两套账来自不同上游、不同计费，混在一起看不出哪边快用完。
        合计保留，下面按域小计（与「账号池」的 realmTotals 同一惯例）。
        """
        result = self.run_frontend("""
S.data={accounts:[
  {uid:'g1',realm:'global',pool:{inPool:true},credits:{remain:100,size:400}},
  {uid:'g2',realm:'global',pool:{inPool:true},credits:{remain:50,size:200}},
  {uid:'c1',realm:'cn',pool:{inPool:true},credits:{remain:30,size:100}}]};
renderCredit();
return $('#creditPanel').innerHTML;
""")
        self.assertIn("国际剩余", result, "应列出国际积分小计")
        self.assertIn("国内剩余", result, "应列出国内积分小计")
        self.assertIn("150", result, "国际小计应为 100+50")
        self.assertIn("30", result, "国内小计应为 30")
        self.assertIn("180", result, "合计仍应为 180")

    def test_overview_credit_skips_absent_realm(self):
        """某域一个号都没取到积分时不显示该行，避免堆空行。"""
        result = self.run_frontend("""
S.data={accounts:[{uid:'g1',realm:'global',pool:{inPool:true},credits:{remain:100,size:400}}]};
renderCredit();
return $('#creditPanel').innerHTML;
""")
        self.assertIn("国际剩余", result)
        self.assertNotIn("国内剩余", result)

    def test_model_picker_shows_multiplier_badges(self):
        """模型选择器必须显示扣费倍率——用户此前不知道各模型倍率是多少。

        倍率来自 /api/models 的 credits 字段（面板透传 /v1/models）。按档位配色：
        免费（0）/ 低（<0.5）/ 高（≥0.5），解析不出倍率时如实标「倍率未知」。
        """
        result = self.run_frontend("""
KS.modelItems=[
  {id:'cn:hy3', credits:'x0.00', vendor:'j', supports_reasoning:true},
  {id:'cn:deepseek-v4.1-flash', credits:'x0.11', vendor:'f', supports_tool_call:true},
  {id:'cn:glm-5.3', credits:'x0.79', vendor:'e'},
  {id:'cn:mystery', vendor:'?'}
];
$('#keyModels').value='cn:hy3, cn:glm-5.3';
renderModelPanel();
renderModelTags();
return {panel:$('#keyModelPanel').innerHTML, tags:$('#keyModelTags').innerHTML};
""", extra_sources=("keys.js",))
        panel, tags = result["panel"], result["tags"]
        self.assertIn("免费", panel, "零倍率应显示「免费」")
        self.assertIn("x0.11", panel, "应显示低倍率原文")
        self.assertIn("x0.79", panel, "应显示高倍率原文")
        self.assertIn("mrate free", panel, "免费档应用 free 配色")
        self.assertIn("mrate low", panel, "低倍率档应用 low 配色")
        self.assertIn("mrate high", panel, "高倍率档应用 high 配色")
        self.assertIn("倍率未知", panel, "解析不出倍率时如实标注，不编造")
        # 已选中的两个模型在面板里打勾、在标签区显示为可移除的 chip。
        self.assertEqual(panel.count("✓"), 2, "已选模型应有对勾")
        self.assertIn("cn:hy3", tags)
        self.assertIn('data-drop-model="cn:glm-5.3"', tags, "标签应带移除按钮")

    def test_model_picker_rate_parsing(self):
        """倍率解析要能吃下上游的各种写法，解析不出返回 null 而不是 0。"""
        result = self.run_frontend("""
return {
  plain: parseRate('x0.79'),
  suffixed: parseRate('x0.11 credits'),
  bare: parseRate('0.06'),
  zero: parseRate('x0.00'),
  empty: parseRate(''),
  missing: parseRate(undefined),
  junk: parseRate('credits'),
  cls_free: rateClass(0),
  cls_low: rateClass(0.11),
  cls_high: rateClass(0.79),
  cls_none: rateClass(null)
};
""", extra_sources=("keys.js",))
        self.assertEqual(result["plain"], 0.79)
        self.assertEqual(result["suffixed"], 0.11)
        self.assertEqual(result["bare"], 0.06)
        self.assertEqual(result["zero"], 0.0)
        self.assertIsNone(result["empty"], "空串不是 0，是未知")
        self.assertIsNone(result["missing"])
        self.assertIsNone(result["junk"])
        self.assertEqual(result["cls_free"], "free")
        self.assertEqual(result["cls_low"], "low")
        self.assertEqual(result["cls_high"], "high")
        self.assertEqual(result["cls_none"], "")


if __name__ == "__main__":
    unittest.main()
