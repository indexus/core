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
