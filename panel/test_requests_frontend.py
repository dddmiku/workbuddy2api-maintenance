#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-25：执行真实明细页面脚本，验证分页、乱序响应、空/零用量、调度解释和HTML转义。
# 2026-09-25：验证写入失败提示与公开的按密钥跳转，确保不可见记录和旧筛选不会被掩盖。
# 2026-09-25：验证调度真实次数、截断提示和末次账号展示，兼容缺省计数字段。

import json
import os
from pathlib import Path
import shutil
import subprocess
import unittest


PANEL = Path(__file__).resolve().parent
NODE = shutil.which("node")
if not NODE and Path("C:/Program Files/nodejs/node.exe").is_file():
    NODE = "C:/Program Files/nodejs/node.exe"


@unittest.skipUnless(NODE, "Node.js is required for request detail behavior checks")
class RequestFrontendTests(unittest.TestCase):
    def run_frontend(self, expression):
        program = r"""
const vm=require('vm'),fs=require('fs'),path=require('path');
const nodes=new Map();
function node(id){
  if(!nodes.has(id)){
    const classes=new Set(['hide']);
    nodes.set(id,{innerHTML:'',textContent:'',value:id==='requestPageSize'?'20':'',disabled:false,
      open:false,attrs:{},events:{},focusCount:0,isConnected:true,
      classList:{add(x){classes.add(x)},remove(x){classes.delete(x)},contains(x){return classes.has(x)},toggle(x,on){if(on===undefined)on=!classes.has(x);on?classes.add(x):classes.delete(x)}},
      setAttribute(k,v){this.attrs[k]=String(v);if(k==='open')this.open=true},getAttribute(k){return this.attrs[k]},removeAttribute(k){delete this.attrs[k];if(k==='open')this.open=false},
      addEventListener(k,fn){this.events[k]=fn},focus(){this.focusCount++},showModal(){this.open=true},
      close(){this.open=false;if(this.events.close)this.events.close()}});
  }
  return nodes.get(id);
}
const context={console,Date,Promise,Map,Set,URLSearchParams,encodeURIComponent,
  setInterval(){throw new Error('request page must not start an auto-refresh timer')},
  location:{hash:'#requests'},addEventListener(){},getSelection(){return ''},
  document:{readyState:'complete',getElementById:node,activeElement:node('opener'),addEventListener(){}},
  testNode:node, api(){throw new Error('fixture must provide an API response')},
  fixtureRow(overrides){return Object.assign({request_id:'req-1',protocol:'responses',model:'cn:deepseek-v4.1-flash',key_id:'key-1',key_name:'调用方',started_at:'2026-09-25T01:02:03Z',finished_at:'2026-09-25T01:02:04Z',recorded_at:'2026-09-25T01:02:04Z',status:'success',http_status:200,duration_ms:1000,queue_ms:0,ttfb_ms:0,upstream_started:true,attempt_count:1,stored_attempts:1,unknown_attempts:0,usage_state:'complete',input_tokens:100,output_tokens:0,cached_tokens:null,reasoning_tokens:null,credit:0,missing_cached_attempts:1,missing_reasoning_attempts:1,missing_credit_attempts:0,stream:true},overrides)},
  fixturePage(items,overrides){return Object.assign({ok:true,items,total:items.length,offset:0,limit:20},overrides)},
  requestClick(id){return {target:{closest(){return {getAttribute(){return id}}}}}}
};
context.window=context;vm.createContext(context);
vm.runInContext(fs.readFileSync(path.join(process.argv[1],'requests.js'),'utf8'),context);
Promise.resolve(vm.runInContext('(async()=>{'+process.argv[2]+'})()',context))
  .then(result=>console.log(JSON.stringify(result)))
  .catch(error=>{console.error(error);process.exitCode=1});
"""
        result = subprocess.run(
            [NODE, "-e", program, str(PANEL), expression],
            capture_output=True, text=True, encoding="utf-8",
            env=dict(os.environ, TZ="Asia/Shanghai"), timeout=10,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def test_unknown_and_actual_zero_are_not_conflated(self):
        result = self.run_frontend("""
api=async()=>fixturePage([fixtureRow({finish_reason:'length',usage_state:'partial',unknown_attempts:1,missing_input_attempts:1,last_decision:{reason_code:'sticky_hit',account_id:'account-1'}})]);
await loadRequests();return {html:testNode('requestRows').innerHTML,status:testNode('requestsStatus').textContent};
""")
        self.assertIn('requests-metric">0<', result["html"])
        self.assertIn("未知", result["html"])
        self.assertIn("0 ms", result["html"])
        self.assertIn("部分用量", result["html"])
        self.assertIn("达到输出上限", result["html"])
        self.assertIn("继续使用会话绑定账号", result["html"])
        self.assertIn("cn:deepseek-v4.1-flash", result["html"])

    def test_list_escapes_values_and_does_not_use_prototype_names(self):
        result = self.run_frontend(r"""
const attack='<img src=x onerror=alert(1)>';
api=async()=>fixturePage([fixtureRow({request_id:'q" onclick="alert(1)',key_id:'__proto__',key_name:attack,model:attack,protocol:'constructor',finish_reason:'toString',last_decision:{reason_code:'constructor',account_id:attack}})]);
await loadRequests();return {html:testNode('requestRows').innerHTML,options:testNode('requestKeyOptions').innerHTML};
""")
        self.assertNotIn("<img", result["html"])
        self.assertNotIn(' onclick="alert', result["html"])
        self.assertIn("&lt;img", result["html"])
        self.assertNotIn("[native code]", result["html"])
        self.assertIn('value="__proto__"', result["options"])
        self.assertNotIn("<img", result["options"])

    def test_filter_query_preserves_values_and_admin_relative_path(self):
        result = self.run_frontend("""
var paths=[];api=async(path)=>{paths.push(path);return fixturePage([])};
testNode('requestKeyFilter').value=' key-1 ';testNode('requestModelFilter').value='global:full/model+version';
testNode('requestStatusFilter').value='error';testNode('requestIDFilter').value='req-9';
await testNode('requestFilters').events.submit({preventDefault(){}});return paths;
""")
        self.assertEqual(len(result), 1)
        self.assertTrue(result[0].startswith("api/requests?"))
        from urllib.parse import parse_qs
        query = parse_qs(result[0].split("?", 1)[1])
        self.assertEqual(query, {"key_id": ["key-1"], "model": ["global:full/model+version"], "status": ["error"], "request_id": ["req-9"], "offset": ["0"], "limit": ["20"]})

    def test_late_filter_response_cannot_replace_current_results(self):
        result = self.run_frontend("""
var release,paths=[];api=(path)=>{paths.push(path);return paths.length===1 ? new Promise(resolve=>{release=resolve}) : Promise.resolve(fixturePage([fixtureRow({request_id:'new-result',model:'new-model'})]));};
var old=loadRequests();testNode('requestModelFilter').value='new-model';
await testNode('requestFilters').events.submit({preventDefault(){}});
release(fixturePage([fixtureRow({request_id:'old-result',model:'old-model'})]));await old;
return {paths:paths,html:testNode('requestRows').innerHTML,busy:testNode('requestsPanel').attrs['aria-busy']};
""")
        self.assertEqual(len(result["paths"]), 2)
        self.assertIn("new-result", result["html"])
        self.assertNotIn("old-result", result["html"])
        self.assertEqual(result["busy"], "false")

    def test_pagination_remains_stable_until_explicit_refresh(self):
        result = self.run_frontend("""
var paths=[];api=async(path)=>{paths.push(path);var q=new URLSearchParams(path.split('?')[1]),offset=Number(q.get('offset'));return fixturePage(Array.from({length:20},(_,i)=>fixtureRow({request_id:'req-'+(offset+i)})),{offset,total:65})};
await loadRequests();await testNode('requestsNext').events.click();var older=testNode('requestRows').innerHTML;
await loadRequests();var afterAutomatic=paths.length;
await testNode('btnRequestsReload').events.click();return {paths,afterAutomatic,older,range:testNode('requestsRange').textContent,page:testNode('requestsPageNumber').textContent};
""")
        self.assertEqual(result["afterAutomatic"], 2)
        self.assertEqual(len(result["paths"]), 3)
        self.assertIn("offset=20", result["paths"][1])
        self.assertIn("offset=0", result["paths"][2])
        self.assertIn("req-20", result["older"])
        self.assertEqual(result["page"], "1")
        self.assertIn("1–20 / 65", result["range"])

    def test_reset_and_page_size_start_from_first_page(self):
        result = self.run_frontend("""
var paths=[];api=async(path)=>{paths.push(path);return fixturePage([],{limit:Number(new URLSearchParams(path.split('?')[1]).get('limit'))})};
testNode('requestKeyFilter').value='old-key';testNode('requestIDFilter').value='old-id';
await testNode('requestFilters').events.submit({preventDefault(){}});
await testNode('requestFilters').events.reset({preventDefault(){}});
testNode('requestPageSize').value='50';await testNode('requestPageSize').events.change();
return {paths,key:testNode('requestKeyFilter').value,id:testNode('requestIDFilter').value};
""")
        self.assertEqual(result["key"], "")
        self.assertEqual(result["id"], "")
        self.assertNotIn("key_id", result["paths"][1])
        self.assertIn("limit=50", result["paths"][2])
        self.assertIn("offset=0", result["paths"][2])

    def test_errors_and_empty_results_are_distinct_and_retryable(self):
        result = self.run_frontend("""
api=async()=>{throw new Error('<script>network failure</script>')};await loadRequests();
var failed={text:testNode('requestsError').textContent,shown:!testNode('requestsError').classList.contains('hide'),button:testNode('btnRequestsReload').disabled,rows:testNode('requestRows').innerHTML,range:testNode('requestsRange').textContent};
api=async()=>fixturePage([]);await testNode('btnRequestsReload').events.click();
return {failed,html:testNode('requestRows').innerHTML,errorHidden:testNode('requestsError').classList.contains('hide'),nextDisabled:testNode('requestsNext').disabled};
""")
        self.assertTrue(result["failed"]["shown"])
        self.assertFalse(result["failed"]["button"])
        self.assertEqual(result["failed"]["text"], "<script>network failure</script>")
        self.assertIn("暂时无法读取明细", result["failed"]["rows"])
        self.assertEqual(result["failed"]["range"], "暂时无法读取条数")
        self.assertTrue(result["errorHidden"])
        self.assertTrue(result["nextDisabled"])
        self.assertIn("没有匹配", result["html"])

    def test_loading_and_disabled_journal_are_visible(self):
        result = self.run_frontend("""
var release;api=()=>new Promise(resolve=>{release=resolve});var pending=loadRequests();
var during={busy:testNode('requestsPanel').attrs['aria-busy'],html:testNode('requestRows').innerHTML,disabled:testNode('btnRequestsReload').disabled};
release({ok:false,message:'请求明细未启用'});await pending;return {during,message:testNode('requestsError').textContent};
""")
        self.assertEqual(result["during"]["busy"], "true")
        self.assertTrue(result["during"]["disabled"])
        self.assertIn("正在加载", result["during"]["html"])
        self.assertEqual(result["message"], "请求明细未启用")

    def test_closing_dialog_invalidates_inflight_detail(self):
        result = self.run_frontend("""
var release;api=()=>new Promise(resolve=>{release=resolve});
var pending=testNode('requestRows').events.click(requestClick('late-detail'));
testNode('requestDetailDone').events.click();release({ok:true,record:fixtureRow({request_id:'late-detail',attempts:[],decisions:[]})});await pending;
return {open:testNode('requestDetailDialog').open,html:testNode('requestDetailBody').innerHTML,id:testNode('requestDetailID').textContent,focus:testNode('opener').focusCount};
""")
        self.assertFalse(result["open"])
        self.assertEqual(result["html"], "")
        self.assertEqual(result["id"], "")
        self.assertEqual(result["focus"], 1)

    def test_late_detail_cannot_replace_new_selection(self):
        result = self.run_frontend("""
var release;api=(path)=>path.endsWith('/old')?new Promise(resolve=>{release=resolve}):Promise.resolve({ok:true,record:fixtureRow({request_id:'new',model:'new-model',attempts:[],decisions:[]})});
var old=testNode('requestRows').events.click(requestClick('old'));await testNode('requestRows').events.click(requestClick('new'));
release({ok:true,record:fixtureRow({request_id:'old',model:'old-model',attempts:[],decisions:[]})});await old;
return {html:testNode('requestDetailBody').innerHTML,id:testNode('requestDetailID').textContent};
""")
        self.assertIn("new-model", result["html"])
        self.assertNotIn("old-model", result["html"])
        self.assertEqual(result["id"], "new")

    def test_detail_preserves_attempt_unknowns_and_actual_decisions(self):
        result = self.run_frontend(r"""
const attack='<img src=x onerror=alert(1)>';
var paths=[];api=async(path)=>{paths.push(path);return {ok:true,record:fixtureRow({request_id:'safe/id?',model:attack,finish_reason:'content_filter',attempt_count:3,attempts_truncated:true,attempts:[
{number:1,status:'error',input_tokens:null,output_tokens:null,credit:null,error_code:'channel_rejected',usage_state:'missing'},
{number:2,status:'success',input_tokens:20,output_tokens:0,cached_tokens:0,reasoning_tokens:null,credit:0,usage_state:'complete',finish_reason:'length',model:attack}],
decisions:[{attempt:1,reason_code:'sticky_unavailable',blocked_by:'model_cooldown',bound_account_id:attack,stage_counts:{pool_total:5,available:0},excluded_counts:{model_cooldown:1,fallback_model_cooldown:1},cost_state:'unknown',cost_unknown_reason:'expired'},
{attempt:2,reason_code:'weighted_selection',selection_method:'weighted',account_id:attack,weight_units:3,weight_total:10,cost_state:'paid',cost_per_1k:0.25,cost_samples:3,cost_used_for_selection:true}]})};};
await testNode('requestRows').events.click(requestClick('safe/id?'));return {paths,html:testNode('requestDetailBody').innerHTML};
""")
        self.assertEqual(result["paths"], ["api/requests/safe%2Fid%3F"])
        self.assertNotIn("<img", result["html"])
        self.assertIn("&lt;img", result["html"])
        for text in ["第 1 次尝试", "第 2 次尝试", "未知", "<b>0</b>", "其余 1 次", "达到输出上限", "内容受到过滤", "会话绑定账号暂不可用", "兜底检查（单独统计）", "本次抽签权重：3 / 10", "这不是本次消费"]:
            self.assertIn(text, result["html"])

    def test_nonweighted_decisions_do_not_invent_lottery_probability(self):
        html = self.run_frontend("""
api=async()=>({ok:true,record:fixtureRow({request_id:'sticky',attempts:[],decisions:[{attempt:0,reason_code:'sticky_hit',selection_method:'sticky',weight_units:3,weight_total:10,cost_state:'free',cost_per_1k:0,cost_used_for_selection:false}]})});
await testNode('requestRows').events.click(requestClick('sticky'));return testNode('requestDetailBody').innerHTML;
""")
        self.assertNotIn("本次抽签权重", html)
        self.assertIn("本次未用于选择", html)

    def test_wrong_detail_identity_shows_error_and_can_retry(self):
        result = self.run_frontend("""
api=async()=>({ok:true,record:fixtureRow({request_id:'wrong',model:'wrong-model'})});
await testNode('requestRows').events.click(requestClick('wanted'));
var failed={html:testNode('requestDetailBody').innerHTML,retry:!testNode('requestDetailRetry').classList.contains('hide')};
api=async()=>({ok:true,record:fixtureRow({request_id:'wanted',model:'correct-model',upstream_started:false,attempts:[],decisions:[]})});await testNode('requestDetailRetry').events.click();
return {failed,html:testNode('requestDetailBody').innerHTML};
""")
        self.assertTrue(result["failed"]["retry"])
        self.assertNotIn("wrong-model", result["failed"]["html"])
        self.assertIn("correct-model", result["html"])
        self.assertIn("未调用上游", result["html"])

    def test_recovery_notice_does_not_imply_full_history(self):
        result = self.run_frontend("""
api=async()=>fixturePage([],{recovery:{count:2,discarded_tail_bytes:15,last_recovered_at:'2026-09-25T01:02:03Z'}});
await loadRequests();return {message:testNode('requestsRecovery').textContent,shown:!testNode('requestsRecovery').classList.contains('hide')};
""")
        self.assertTrue(result["shown"])
        self.assertIn("恢复 2 次", result["message"])
        self.assertIn("此前完整记录", result["message"])

    def test_recording_errors_are_visible_alongside_tail_recovery(self):
        result = self.run_frontend("""
api=async()=>fixturePage([],{recording_errors:3,recovery:{count:1,last_recovered_at:'2026-09-25T01:02:03Z'}});
await loadRequests();return {message:testNode('requestsRecovery').textContent,shown:!testNode('requestsRecovery').classList.contains('hide'),warningStyle:testNode('requestsRecovery').classList.contains('w')};
""")
        self.assertTrue(result["shown"])
        self.assertTrue(result["warningStyle"])
        self.assertIn("3 次明细写入失败", result["message"])
        self.assertIn("列表可能不完整", result["message"])
        self.assertIn("恢复 1 次", result["message"])

    def test_old_recording_error_warning_clears_on_fresh_response(self):
        result = self.run_frontend("""
api=async()=>fixturePage([],{recording_errors:2});await loadRequests();
api=async()=>fixturePage([],{recording_errors:0});await loadRequests(true);
return {hidden:testNode('requestsRecovery').classList.contains('hide'),errorHidden:testNode('requestsError').classList.contains('hide')};
""")
        self.assertTrue(result["hidden"])
        self.assertTrue(result["errorHidden"])

    def test_public_key_navigation_clears_other_filters_and_old_response(self):
        result = self.run_frontend(r"""
var release,paths=[],view='';
go=function(name){view=name;return loadRequests()};
api=(path)=>{paths.push(path);return paths.length===1?new Promise(resolve=>{release=resolve}):Promise.resolve(fixturePage([fixtureRow({request_id:'selected-key-row',key_id:'synthetic-key-2',key_name:'selected'})]));};
var old=loadRequests();testNode('requestModelFilter').value='old-model';testNode('requestStatusFilter').value='error';testNode('requestIDFilter').value='old-id';
await openRequestsForKey('synthetic-key-2','<img src=x onerror=alert(1)>');
release(fixturePage([fixtureRow({request_id:'old-unfiltered'})]));await old;
return {paths,view,key:testNode('requestKeyFilter').value,model:testNode('requestModelFilter').value,status:testNode('requestStatusFilter').value,id:testNode('requestIDFilter').value,html:testNode('requestRows').innerHTML,options:testNode('requestKeyOptions').innerHTML};
""")
        self.assertEqual(result["view"], "requests")
        self.assertEqual(result["key"], "synthetic-key-2")
        self.assertEqual(result["model"], "")
        self.assertEqual(result["status"], "")
        self.assertEqual(result["id"], "")
        self.assertEqual(len(result["paths"]), 2)
        self.assertIn("key_id=synthetic-key-2", result["paths"][1])
        self.assertNotIn("model=", result["paths"][1])
        self.assertIn("selected-key-row", result["html"])
        self.assertNotIn("old-unfiltered", result["html"])
        self.assertNotIn("<img", result["options"])

    def test_decision_truncation_shows_actual_total_and_last_account(self):
        result = self.run_frontend("""
api=async(path)=>path.includes('?')?fixturePage([fixtureRow({decision_count:65,decisions_truncated:true,last_decision:{attempt:65,reason_code:'cooldown_fallback',account_id:'actual-last'}})]):{ok:true,record:fixtureRow({request_id:'req-1',decision_count:65,decisions_truncated:true,attempts:[],decisions:[{attempt:1,reason_code:'weighted_selection',account_id:'prefix-1'},{attempt:65,reason_code:'cooldown_fallback',account_id:'actual-last'}]})};
await loadRequests();var table=testNode('requestRows').innerHTML;
await testNode('requestRows').events.click(requestClick('req-1'));
return {table,detail:testNode('requestDetailBody').innerHTML};
""")
        self.assertIn("65 次调度", result["table"])
        self.assertIn("明细已截断", result["table"])
        self.assertIn("actual-last", result["table"])
        self.assertNotIn("prefix-1", result["table"])
        self.assertIn("共 65 次调度，保留 2 条记录", result["detail"])
        self.assertIn("调度历史已截断", result["detail"])

    def test_legacy_detail_derives_decision_total_from_retained_array(self):
        html = self.run_frontend("""
api=async()=>({ok:true,record:fixtureRow({request_id:'legacy',attempts:[],decisions:[{attempt:1,reason_code:'sticky_unavailable'},{attempt:1,reason_code:'weighted_selection'}]})});
await testNode('requestRows').events.click(requestClick('legacy'));return testNode('requestDetailBody').innerHTML;
""")
        self.assertIn("共 2 次调度", html)
        self.assertNotIn("调度历史已截断", html)


if __name__ == "__main__":
    unittest.main()
