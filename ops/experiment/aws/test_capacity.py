"""Fleet planning tests: no network or AWS resources."""
import contextlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import capacity


class CapacityTests(unittest.TestCase):
    def config(self, **changes):
        return dict(dict(hosts=2, slots=2, loaders=1, ttl=45, budget='6', max_hosts=None), **changes)

    def bound(self, config, quota=512, other=0):
        return capacity.bound(config, 4, 16, quota, other, '.398', '.808')

    def test_budget_accounts_for_loader_type_and_duration(self):
        p = self.bound(self.config())
        self.assertEqual((p['max_machines'], p['max_vnodes'], p['reason']), (18, 36, 'budget'))

    def test_operator_limit_can_be_removed_beyond_32_vnodes(self):
        config = self.config(budget='20', max_hosts=16)
        self.assertEqual(self.bound(config)['max_vnodes'], 32)
        config['max_hosts'] = None
        self.assertEqual(self.bound(config)['max_vnodes'], 128)

    def test_quota_subtracts_other_campaigns_and_real_loader_cpus(self):
        p = self.bound(self.config(budget='1000'), other=64)
        self.assertEqual((p['max_machines'], p['reason']), (108, 'quota'))

    def test_no_hidden_128_machine_limit(self):
        self.assertEqual(self.bound(self.config(budget='1000'), quota=4096)['max_machines'], 1020)

    def test_resource_admission_has_no_vnode_count_limit(self):
        result = self.bound(self.config(slots=0, resource_slots=True))
        self.assertIsNone(result['max_vnodes'])
        self.assertGreater(result['max_machines'], 0)
        with self.assertRaises(ValueError):
            self.bound(self.config(slots=0, resource_slots=False))

    def test_initial_fleet_must_fit_all_bounds(self):
        for config, quota in [(self.config(budget='.01'), 512), (self.config(), 16),
                              (self.config(max_hosts=1), 512)]:
            with self.assertRaises(ValueError):
                self.bound(config, quota=quota)

    def test_peak_fleet_requirement_cannot_be_silently_capped(self):
        config = self.config(hosts=12, ttl=20, budget='6.6', min_peak_hosts=90)
        with self.assertRaisesRegex(ValueError, r"requires 90 data hosts, ceiling=45"):
            capacity.bound(config, 4, 32, 512, 2, '.398', '1.6968')
        config['budget'] = '12.51'
        self.assertEqual(capacity.bound(config, 4, 32, 512, 2, '.398', '1.6968')['max_machines'], 90)
        with self.assertRaisesRegex(ValueError, "ceiling=60"):
            self.bound(dict(config, max_hosts=60))

    def test_invalid_numeric_configuration(self):
        for budget in ('0', '-1', 'NaN', 'Infinity'):
            with self.assertRaises(ValueError):
                self.bound(self.config(budget=budget))

    def test_plan_excludes_own_campaign_and_spot_instances(self):
        config = self.config(run='test', region='eu-west-3', host_type='i4i.xlarge',
                             loader_type='c6i.4xlarge', host_price='.398', loader_price='.808')
        def aws(region, service, operation, *args):
            if operation == 'get-service-quota':
                return {'Quota': {'Value': 512}}
            if operation == 'describe-instances':
                return {'Reservations': [{'Instances': [
                    {'InstanceType': 'i4i.xlarge', 'Tags': [{'Key': 'Run', 'Value': 'test'}]},
                    {'InstanceType': 'i4i.xlarge', 'InstanceLifecycle': 'spot'},
                    {'InstanceType': 'c6i.4xlarge'}]}]}
            return {'InstanceTypes': [dict(InstanceType=k, VCpuInfo={'DefaultVCpus': n})
                                      for k, n in [('i4i.xlarge', 4), ('c6i.4xlarge', 16)]]}
        with patch.object(capacity, 'aws', side_effect=aws):
            self.assertEqual(capacity.plan(config)['other_vcpus'], 16)

    def test_live_update_dry_run_does_not_publish_or_modify_state(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / 'run.capacity.json'
            config = self.config(run='test', region='eu-west-3', bucket='test',
                                 host_type='i4i.xlarge', loader_type='c6i.4xlarge',
                                 host_price='.398', loader_price='.808', max_hosts=16, resource_slots=True,
                                 min_peak_hosts=12)
            original = json.dumps(config)
            state.write_text(original)
            with patch.object(sys, 'argv', ['capacity.py', 'update', str(state),
                              '--budget', '20', '--max-hosts', 'auto', '--dry-run']), \
                 patch.object(capacity, 'plan', side_effect=lambda c: self.bound(c)), \
                 patch.object(capacity.subprocess, 'run') as publish, contextlib.redirect_stdout(io.StringIO()) as out:
                capacity.main()
                publish.assert_not_called()
            self.assertEqual(state.read_text(), original)
            self.assertEqual(json.loads(out.getvalue())['max_vnodes'], 128)
            self.assertTrue(json.loads(out.getvalue())['resource_slots'])
            self.assertEqual(json.loads(out.getvalue())['min_peak_hosts'], 12)

    def test_failed_policy_publication_keeps_local_state(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / 'run.capacity.json'
            config = self.config(run='test', region='eu-west-3', bucket='test',
                                 host_type='i4i.xlarge', loader_type='c6i.4xlarge',
                                 host_price='.398', loader_price='.808')
            original = json.dumps(config)
            state.write_text(original)
            with patch.object(sys, 'argv', ['capacity.py', 'update', str(state), '--budget', '20']), \
                 patch.object(capacity, 'plan', side_effect=lambda c: self.bound(c)), \
                 patch.object(capacity.subprocess, 'run', side_effect=subprocess.CalledProcessError(1, 'aws')):
                with self.assertRaises(subprocess.CalledProcessError):
                    capacity.main()
            self.assertEqual(state.read_text(), original)

    def test_node_role_cannot_raise_its_own_capacity_policy(self):
        policy = json.loads(subprocess.check_output(
            [sys.executable, str(Path(__file__).with_name('template.py')), 's3', 'test']))
        writable = [r for s in policy['Statement'] if s['Action'] == 's3:PutObject' for r in s['Resource']]
        self.assertEqual(set(writable), {'arn:aws:s3:::test/results/*',
                         'arn:aws:s3:::test/agents', 'arn:aws:s3:::test/agents.d/*'})


if __name__ == '__main__':
    unittest.main()
