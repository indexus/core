import unittest
from fleet import Fleet
import live

class FleetTests(unittest.TestCase):
    def sample(self, applied=10, uptime=10, name='v'):
        return {'http://10.0.0.1:22001':dict(name=name,uptime_seconds=uptime,writes=dict(applied=applied),storage=dict(items=applied))}

    def test_rates_belong_to_incarnation_and_exclude_counter_reset(self):
        f=Fleet()
        def rate(m,t):return f.frame(m,[],t)['vnodes'][-1]['applied_s']
        self.assertIsNone(rate(self.sample(),100))
        self.assertEqual(rate(self.sample(20,20),110),1)
        self.assertIsNone(rate(self.sample(2,1),120))
        self.assertIsNone(rate(self.sample(50,20,'replacement'),130))

    def test_missing_nodes_and_resources_stay_unknown_not_zero(self):
        f=Fleet();f.frame(self.sample(),[],100)
        got=f.frame({},[],120)
        self.assertEqual(got['vnodes'][0]['state'],'stale')
        self.assertIsNone(got['vnodes'][0]['applied_s'])
        self.assertIsNone(got['machines'][0]['cpu'])

    def test_rate_uses_sample_interval_not_aggregator_clock(self):
        f=Fleet()
        f.frame(self.sample(1000,10),[],100)
        # The five-second aggregator sees successive samples four then six
        # seconds apart. Both represent 100 actual applications per second.
        a=f.frame(self.sample(1400,14),[],105)
        b=f.frame(self.sample(2000,20),[],110)
        self.assertEqual(a['vnodes'][0]['applied_s'],100)
        self.assertEqual(b['vnodes'][0]['applied_s'],100)

    def test_ready_machine_requires_fresh_resource_sample(self):
        f=Fleet();m=dict(agent='http://10.0.0.1:22100',wall=100,facts=dict(receiver_ready=True,running=0,free=5,cores=4,cpu_ok=True,cpu_pct=2,mem_ok=True,mem_free_pct=90,disk_ok=True,disk_free_pct=90))
        self.assertEqual(f.frame({},[m],105)['machines'][0]['state'],'warm')
        h=f.frame({},[],150)['machines'][0]
        self.assertEqual(h['state'],'unknown');self.assertIsNone(h['cpu'])

    def test_stale_transfer_is_retained_with_unknown_freeze_state(self):
        f=Fleet();m=self.sample();next(iter(m.values()))['handoffs']=dict(open=dict(list=[dict(op='op',target='https://10.0.0.2:1234',phase='copy')]))
        f.frame(m,[],100)
        tr=f.frame({},[],120)['transfers'][0]
        self.assertTrue(tr['stale']);self.assertIsNone(tr['progress']);self.assertIsNone(tr['frozen'])

    def test_new_application_counter_is_not_inferred_from_entry_acks(self):
        f=Fleet();m=self.sample();w=next(iter(m.values()))['writes'];del w['applied'];w['acknowledged']=10000
        f.frame(m,[],100);m= self.sample();del next(iter(m.values()))['writes']['applied']
        self.assertIsNone(f.frame(m,[],110)['vnodes'][0]['applied_s'])

    def test_client_read_histogram_and_unknown_reads_are_distinct(self):
        p={};r=dict(t=1,offered=1,acked=1,shed=0,reads=3,read_failed=1,read_latency_us_log2=[0,3])
        live.build(p,[r],{},None)
        self.assertEqual(p['reads_s'],3);self.assertEqual(p['read_failed_s'],1);self.assertEqual(p['read_p99_ms'],.002)
        del r['reads'];live.build(p,[r],{},None);self.assertIsNone(p['reads_s'])

    def test_embedded_names_cannot_close_script(self):
        p=live.fleet_page([dict(wall=1,phase='</script><script>alert(1)</script>')],'test',False)
        self.assertNotIn('"phase":"</script>',p)
        self.assertIn('\\u003c/script>',p)
