import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import fs from 'node:fs';
async function fixture(available=3,slots=[1,2,3]){
 let clock=1000,reads=0,handler;
 class ClockDate extends Date {constructor(...a){super(...(a.length?a:[clock]));}static now(){return clock;}}
 const http={createServer(fn){handler=fn;return {listen(){},close(fn){fn();}};}};
 const context=vm.createContext({Date:ClockDate,AbortSignal,console:{log(){}},process:{env:{},argv:['node','forward.mjs'],on(){}},fetch:async url=>{
  reads++;const u=new URL(url),i=Number(u.port)-19031;assert.ok(slots.includes(i+1));
  return {ok:i<available,json:async()=>u.pathname==='/status'?{name:'node'+i}:{Collection00001:{'@':true}}};
 }});
 const source=process.env.INDEXUS_FORWARD_TEST_SOURCE??new URL('./forward.mjs',import.meta.url);
 const module=new vm.SourceTextModule(fs.readFileSync(source,'utf8'),{context});
 await module.link(name=> name==='node:http' ? new vm.SyntheticModule(['default'],function(){this.setExport('default',http);},{context}) : new vm.SyntheticModule(['readdir'],function(){this.setExport('readdir',async()=>slots.map(i=>({isDirectory:()=>true,name:'node'+i})));},{context}));await module.evaluate();
 return {reads:()=>reads,advance:ms=>{clock+=ms;},recover:()=>{available=3;},async request(method='GET'){
  let status,body;await handler({url:'/api/demo/mesh',method},{writeHead(n){status=n;},end(s){body=s;}});
  return {status,nodes:body?JSON.parse(body).nodes:[]};
 }};
}
for(const available of [0,2])test(`inventory with ${available} available nodes retries after five seconds`,async()=>{
 const f=await fixture(available);assert.equal((await f.request()).nodes.filter(n=>n.available).length,available);f.recover();
 f.advance(4999);await f.request();assert.equal(f.reads(),6);
 f.advance(2);assert.equal((await f.request()).nodes.filter(n=>n.available).length,3);assert.equal(f.reads(),12);
});
test('complete snapshots retain their minute cache and concurrent reads coalesce',async()=>{
 const f=await fixture();await Promise.all(Array.from({length:8},()=>f.request()));assert.equal(f.reads(),6);
 f.advance(59999);await f.request();assert.equal(f.reads(),6);f.advance(2);await f.request();assert.equal(f.reads(),12);
});
test('inventory POST is refused without monitoring reads',async()=>{
 const f=await fixture();assert.equal((await f.request('POST')).status,405);assert.equal(f.reads(),0);
});

test('inventory follows the actual local vnode slots',async()=>{
 const f=await fixture(8,[1,4,7]);
 assert.deepEqual((await f.request()).nodes.map(n=>n.label),['N1','N4','N7']);
});
