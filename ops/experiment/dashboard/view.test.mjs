import test from 'node:test';
import assert from 'node:assert/strict';
import './view.js';

const {growthMetrics}=globalThis.IndexusDashboard;
test('growth uses client readiness and reporting machines at the same instant',()=>{
  const point={nodes:102, ready_nodes:76, vnodes:89, fleet:{machines:Array.from({length:32},(_,i)=>({id:i,state:'active'}))}};
  assert.deepEqual(growthMetrics(point),{observed_count:102,ready_count:76,machine_count:32,ready_per_machine:76/32});
  assert.equal(growthMetrics({...point,ready_nodes:0}).ready_per_machine,0);
  assert.equal(growthMetrics({...point,metrics_stale:true}).ready_per_machine,null);
  assert.equal(growthMetrics({vnodes:32}).ready_count,null);
  point.fleet.machines[0].state='unknown';
  assert.equal(growthMetrics(point).ready_per_machine,null);
});
test('a simulated receiver can already serve some writes during its copy',()=>{
  const fleet={source:'simulation',machines:[{state:'active'}],vnodes:[
    {state:'ready'}, {state:'receiving',estimated_s:12}, {state:'receiving',estimated_s:0}]};
  assert.equal(growthMetrics({fleet}).ready_per_machine,2);
});
test('stock sums reporting vnodes, preserving measured zero and missing coverage',()=>{
  const {itemSum}=globalThis.IndexusDashboard;
  const nodes=[{items:17},{items:0},{state:'stale',items:99},{items:0,items_counted:false},{}];
  assert.deepEqual(itemSum(nodes,false),{value:17,count:2,total:5});
  assert.equal(itemSum(nodes,true).value,null);
  assert.equal(itemSum([{items:0}],false).value,0);
});
test('radial layout uses the Indexus 96-bit alphabet and XOR bucket depths',()=>{
  const {addressBits,commonBits}=globalThis.IndexusDashboard;
  const zero=addressBits('0'.repeat(16));
  assert.equal(zero,'0'.repeat(96));
  assert.equal(commonBits(zero,addressBits('0'.repeat(15)+'1')),95);
  assert.equal(commonBits(zero,addressBits('_'.repeat(16))),0);
  assert.equal(addressBits('V1'),'');
});
test('load spread excludes a machine with incomplete counters',()=>{
  const {hostSpread}=globalThis.IndexusDashboard;
  const p={fleet:{vnodes:[{machine:'a',applied_s:2,items:5},{machine:'a',applied_s:4,items:7},
    {machine:'b',applied_s:null,items:1},{machine:'c',applied_s:0,items:0}]}};
  assert.deepEqual(hostSpread(p),{hostRateLow:0,hostRateMedian:3,hostRateHigh:6,hostItemsLow:0,hostItemsMedian:1,hostItemsHigh:12});
  assert.deepEqual(hostSpread({...p,metrics_stale:true}),{});
});
test('only a fresh donor can confirm an operation left its reported list',()=>{
  const {departedTransfers}=globalThis.IndexusDashboard;
  const before={fleet:{transfers:[{id:'done',source:'A'},{id:'unknown',source:'B'},{id:'active',source:'A'}]}};
  const now={fleet:{vnodes:[{id:'A',state:'ready'},{id:'B',state:'stale'}],transfers:[{id:'active',source:'A'}]}};
  assert.equal(departedTransfers(before,now),1);
  assert.equal(departedTransfers(before,{...now,metrics_stale:true}),null);
});
test('XOR shares split only on branching bits, including near-identical addresses',()=>{
  const a='0'.repeat(16),b='0'.repeat(15)+'1',c='_'.repeat(16);
  const shares=globalThis.IndexusDashboard.xorShares([b,c,a,a]);
  assert.deepEqual([...shares],[[a,.25],[b,.25],[c,.5]]);
});
test('vnode scope sums observed applications and counts an internal transfer once',()=>{
  const {selectedPoint}=globalThis.IndexusDashboard;
  const p={x:5,fleet:{vnodes:[{id:'a',label:'A',applied_s:4,items:5},{id:'b',label:'B',monitor:'dst',applied_s:0,items:2}],transfers:[{source:'a',target:'dst',frozen:true}]}};
  const ids=new Set(['A','B']);
  assert.deepEqual(selectedPoint(p,ids),{x:5,t:undefined,rate:4,stock:7,open:1,frozen:1,ready:0,reporting:2,cpu:null,ram:null,disk:null,peakRate:4});
  p.fleet.vnodes[1].state='stale';
  assert.equal(selectedPoint(p,ids).rate,null);
  assert.equal(selectedPoint(p,ids).open,null);
});
