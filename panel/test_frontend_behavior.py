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
function node(id) {
  if (!nodes.has(id)) nodes.set(id, {innerHTML:'', textContent:'', value:'', disabled:false,
    style:{}, attrs:{}, events:{}, classList:{add(){},remove(){},toggle(){}},
    setAttribute(k,v){this.attrs[k]=String(v)}, getAttribute(k){return this.attrs[k]},
    removeAttribute(k){delete this.attrs[k]}, addEventListener(k,v){this.events[k]=v},
    querySelector(s){return node(id+' '+s)},querySelectorAll(){return []}});
  return nodes.get(id);
}
const context = {console, Date, Promise, Map,
  setTimeout(){return 1},clearTimeout(){},setInterval(){return 1},clearInterval(){},
  location:{origin:'http://fixture.invalid',hash:'#overview',replace(){}},
  history:{replaceState(){}},localStorage:{getItem(){return null},setItem(){}},
  document:{readyState:'loading',hidden:false,documentElement:node('html'),
    querySelector:node,querySelectorAll(){return []},addEventListener(){}}};
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


if __name__ == "__main__":
    unittest.main()


    def test_update_page_reports_a_disabled_gateway(self):
        result = self.run_frontend("""
UP.data={ok:true,status:{enabled:false,current:'v2.4.10',commit:'111b07023e0a',state:'idle'}};
renderUpdate();
return {apply:$('#btnUpdApply').disabled, check:$('#btnUpdCheck').disabled,
        state:$('#updState').textContent, hint:$('#updHint').textContent,
        body:$('#updBody').innerHTML};
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
