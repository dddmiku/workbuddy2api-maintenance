#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# ═══ 更新日志 ═══
# 2026-09-25：执行真实密钥页面脚本，验证限流输入与清空、未知消费显示和旧响应不覆盖新设置。

import json
from pathlib import Path
import shutil
import subprocess
import unittest

PANEL = Path(__file__).resolve().parent
NODE = shutil.which("node")


@unittest.skipUnless(NODE, "Node.js is required for key limit frontend checks")
class KeyLimitsFrontendTests(unittest.TestCase):
    def evaluate(self, expression):
        program = r"""
const fs=require('fs'),vm=require('vm'),path=require('path'),nodes=new Map();
function node(id){
 if(!nodes.has(id)){
  let value='';
  nodes.set(id,{innerHTML:'',textContent:'',disabled:false,open:false,checked:false,style:{},attrs:{},events:{},
   get value(){return value},set value(x){value=String(x)},
   classList:{add(){},remove(){},toggle(){}},
   addEventListener(k,f){this.events[k]=f},setAttribute(k,v){this.attrs[k]=v},getAttribute(k){return this.attrs[k]},
   querySelector(s){return node(id+' '+s)},querySelectorAll(){return []},focus(){},showModal(){this.open=true},close(){this.open=false}});
 }
 return nodes.get(id);
}
const c={console,Date,Promise,Map,setTimeout(){},clearTimeout(){},setInterval(){},clearInterval(){},
 location:{origin:'https://fixture.invalid',hash:'#keys'},history:{replaceState(){}},localStorage:{getItem(){return null},setItem(){}},
 document:{readyState:'loading',documentElement:node('html'),querySelector:node,querySelectorAll(){return []},addEventListener(){}}};
c.window=c;vm.createContext(c);
for(const name of ['app.js','usage.js','keys.js'])vm.runInContext(fs.readFileSync(path.join(process.argv[1],name),'utf8'),c);
Promise.resolve(vm.runInContext('(async()=>{'+process.argv[2]+'})()',c)).then(v=>console.log(JSON.stringify(v))).catch(e=>{console.error(e);process.exitCode=1});
"""
        result = subprocess.run([NODE, "-e", program, str(PANEL), expression], capture_output=True,
                                text=True, encoding="utf-8", timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def test_clear_is_explicit_null_and_valid_policy_is_exact(self):
        result = self.evaluate("""
$('#keyRPM').value='';$('#keyConcurrency').value='0';$('#keyQueueSeconds').value='0';
var clear=selectedKeyLimits();
$('#keyRPM').value='120';$('#keyConcurrency').value='3';$('#keyQueueSeconds').value='8';
return {clear,policy:selectedKeyLimits()};
""")
        self.assertEqual(result["clear"], {"value": None})
        self.assertEqual(result["policy"]["value"], {"requests_per_minute": 120, "max_concurrent": 3, "queue_timeout_seconds": 8})

    def test_invalid_inputs_and_queue_without_concurrency_are_rejected(self):
        result = self.evaluate("""
var values=[];
for(var value of ['-1','1.5','1e3','60001','true']){
 $('#keyRPM').value=value;$('#keyConcurrency').value='0';$('#keyQueueSeconds').value='0';values.push(!!selectedKeyLimits().error);
}
$('#keyRPM').value='0';$('#keyQueueSeconds').value='1';values.push(selectedKeyLimits().field==='keyConcurrency');
return values;
""")
        self.assertEqual(result, [True] * 6)

    def test_editor_refills_original_policy(self):
        result = self.evaluate("""
KS.models=[];openKeyEditor({id:'fixture',name:'fixture',limits:{requests_per_minute:11,max_concurrent:2,queue_timeout_seconds:4}});
return [$('#keyRPM').value,$('#keyConcurrency').value,$('#keyQueueSeconds').value];
""")
        self.assertEqual(result, ["11", "2", "4"])

    def test_unknown_total_never_becomes_zero(self):
        result = self.evaluate("""
var key={id:'fixture',name:'fixture',enabled:true,total_tokens:null};KS.keys=[key];renderKeys();var unknown=$('#keyRows').innerHTML;
key.total_tokens=0;renderKeys();return {unknown,zero:$('#keyRows').innerHTML};
""")
        self.assertIn('title="累计用量暂不可读">—', result["unknown"])
        self.assertIn('class="mono key-tokens">0</td>', result["zero"])

    def test_snapshot_error_does_not_hide_keys_or_claim_zero_active(self):
        result = self.evaluate("""
api=async function(path){if(path==='api/key-limits')throw Error('down');return {ok:true,keys:[{id:'fixture',name:'fixture',limits:{max_concurrent:2}}],usage_available:false};};
await loadKeys();return {keys:KS.keys.length,html:$('#keyRows').innerHTML,message:$('#keyLimitsError').textContent};
""")
        self.assertEqual(result["keys"], 1)
        self.assertIn("占用待刷新", result["html"])
        self.assertNotIn("在途 0", result["html"])
        self.assertIn("累计用量暂不可读", result["message"])

    def test_inflight_old_refresh_cannot_restore_old_policy(self):
        result = self.evaluate("""
var resolve,round=0;
api=function(path){if(path==='api/key-limits')return Promise.resolve({ok:true,keys:{}});
 round++;if(round===1)return new Promise(r=>{resolve=r});return Promise.resolve({keys:[{id:'fixture',name:'new',limits:{max_concurrent:5}}]});};
var pending=loadKeys();KS.revision++;resolve({keys:[{id:'fixture',name:'old',limits:{max_concurrent:1}}]});await pending;
for(var i=0;i<6;i++)await Promise.resolve();
return {name:KS.keys[0].name,concurrency:KS.keys[0].limits.max_concurrent};
""")
        self.assertEqual(result, {"name": "new", "concurrency": 5})


if __name__ == "__main__":
    unittest.main()
