"""Execute the real spawn hook with fake AWS/agents, never real resources."""
from concurrent.futures import ThreadPoolExecutor
import json
import fcntl
from datetime import datetime, timezone
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest

SCRIPT = Path(__file__).with_name('spawn.sh').resolve()


class SpawnTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / 'agents').write_text('\n'.join(f'http://host{i}' for i in range(16)))
        self.policy(16)
        (self.root / 'fleet').write_text('16')
        (self.root / 'count').write_text('2 0')
        self.command('aws', '''#!/usr/bin/env python3
import json,os,pathlib,sys,datetime
p=pathlib.Path(os.environ['MOCK_ROOT']); a=sys.argv[1:]
with (p/'aws_calls').open('a') as f: f.write(' '.join(a[:2])+'\\n')
if a[:2] == ['s3','cp']:
 print((p/'policy').read_text())
elif a[:2] == ['ec2','describe-instances']:
 assert 'Name=tag:Name,Values=test-h*' in a, 'must exclude load machines'
 count=int((p/'fleet').read_text()); now=datetime.datetime.now(datetime.timezone.utc).isoformat()
 print(json.dumps([[f'host{i}',now,'running',''] for i in range(count)]))
elif a[:2] == ['ec2','run-instances']:
 with (p/'launches').open('a') as f: f.write(json.dumps(a)+'\\n')
 if os.environ.get('MOCK_ACCEPT'):
  temporary = p/'fleet.tmp'; temporary.write_text(str(int((p/'fleet').read_text())+int(a[a.index('--count')+1])))
  os.replace(temporary, p/'fleet')
 sys.exit(1) # simulate an uncertain response after acceptance, or immediate failure
else:
 sys.exit(2)
''')
        self.command('curl', '''#!/usr/bin/env python3
import os,pathlib,sys
p=pathlib.Path(os.environ['MOCK_ROOT'])
with (p/'curl_calls').open('a') as f: f.write(sys.argv[-1]+'\\n')
if sys.argv[-1].endswith('/count'): print((p/'count').read_text())
elif sys.argv[-1].endswith('/facts'):
 if sys.argv[-1] == 'http://host1/facts': sys.exit(22) # an old agent: 404
 print((p/'facts').read_text())
else:
 with (p/'spawned').open('a') as f: f.write(sys.argv[-1]+'\\n')
 if os.environ.get('MOCK_FULL'): sys.exit(22)
 print('local-33')
''')
        self.command('sleep', '#!/bin/sh\nexit 0\n')
        self.env = dict(os.environ, MOCK_ROOT=str(self.root), INDEXUS_ELASTIC_DIR=str(self.root),
                        PATH=str(self.root)+os.pathsep+os.environ['PATH'], CAPACITY_S3='s3://test/policy',
                        EXPERIMENT_RUN='test', LAUNCH_TEMPLATE='test-host', SLOTS='2')

    def command(self, name, content):
        p = self.root / name
        p.write_text(content)
        p.chmod(0o755)

    def policy(self, maximum, slots=2):
        (self.root / 'policy').write_text(json.dumps(dict(max_machines=maximum, slots=slots, reason='operator')))

    def run_hook(self):
        return subprocess.run(['bash', str(SCRIPT)], env=self.env, text=True, capture_output=True, timeout=10)

    def test_resource_reserve_replaces_slot_ratio_and_counts_boots_once(self):
        self.policy(64)
        self.env['INDEXUS_READY_RESERVE_DEFICIT']='3'
        stamps=self.root/'booting.d';stamps.mkdir()
        (stamps/'accepted.boot').write_text(str(int(time.time())))
        self.run_hook()
        deadline=time.monotonic()+3
        while not (self.root/'launches').exists() and time.monotonic()<deadline:
            time.sleep(.01)
        args=json.loads((self.root/'launches').read_text().splitlines()[0])
        self.assertEqual(int(args[args.index('--count')+1]),2)

    def test_live_policy_lifts_32_vnode_limit(self):
        first = self.run_hook()
        self.assertNotEqual(first.returncode, 0)
        self.assertIn('capacity=16', first.stderr)
        self.assertFalse((self.root / 'launches').exists())
        self.policy(64)
        second = self.run_hook()
        self.assertIn('capacity=64', second.stderr)
        deadline = time.monotonic() + 3
        while not (self.root / 'launches').exists() and time.monotonic() < deadline:
            time.sleep(.01)
        launches = [json.loads(line) for line in (self.root / 'launches').read_text().splitlines()]
        self.assertEqual(len(launches), 1)
        self.assertGreater(int(launches[0][launches[0].index('--count')+1]), 0)
        self.assertIn('--client-token', launches[0])

    def test_unregistered_pending_machines_count_against_limit(self):
        self.policy(64)
        (self.root / 'fleet').write_text('64')
        result = self.run_hook()
        self.assertIn('accounted=64', result.stderr)
        self.assertFalse((self.root / 'launches').exists())

    def test_available_slot_still_usable_at_machine_limit(self):
        (self.root / 'count').write_text('1 1')
        result = self.run_hook()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), 'local-33')
        self.assertFalse((self.root / 'launches').exists())

    def spawned(self):
        path = self.root / 'spawned'
        return path.read_text().split() if path.exists() else []

    def place(self, kind, agent='', machines=16, cap_agent='', reason='why'):
        self.env.update(INDEXUS_PLACE_KIND=kind, INDEXUS_PLACE_AGENT=agent, INDEXUS_PLACE_MACHINES=str(machines),
                        INDEXUS_PLACE_REASON=reason, INDEXUS_PLACE_CAP_AGENT=cap_agent,
                        INDEXUS_PLACE_CAP_REASON='machine cap reached')

    def test_without_decision_the_most_free_slots_rule_stands(self):
        (self.root / 'count').write_text('1 1')
        result = self.run_hook()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.spawned(), ['http://host0/spawn?prefer_near='])
        self.assertIn('placed kind=slots agent=http://host0', result.stderr)

    def test_decision_chooses_the_agent(self):
        (self.root / 'count').write_text('1 1')
        self.place('existing', 'http://host7')
        result = self.run_hook()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), 'local-33')
        self.assertEqual(self.spawned(), ['http://host7/spawn?prefer_near='])
        self.assertIn('placed kind=existing agent=http://host7', result.stderr)
        self.assertIn('place=existing chosen=http://host7 why=why', (self.root / 'machines.log').read_text())

    def test_named_agent_and_zero_deficit_only_contact_that_agent(self):
        # Include a remote registry and a held fleet lock: neither is needed.
        (self.root / 'agents').unlink()
        self.env.update(AGENTS_S3='s3://test/agents', INDEXUS_READY_RESERVE_DEFICIT='0',
                        INDEXUS_PREFER_NEAR='target')
        (self.root / 'booting.d' / '.calc.lock').mkdir(parents=True)
        for kind, full in [('existing', False), ('new', True)]:
            with self.subTest(kind=kind, full=full):
                self.place(kind, 'http://host7')
                self.env['MOCK_FULL'] = '1' if full else ''
                result = self.run_hook()
                self.assertEqual(result.returncode, 22 if full else 0, result.stderr)
                self.assertEqual(result.stdout.strip(), '' if full else 'local-33')
                self.assertEqual((self.root / 'aws_calls').read_text().splitlines(), ['s3 cp'])
                self.assertEqual((self.root / 'curl_calls').read_text().splitlines(),
                                 ['http://host7/spawn?prefer_near=target'])
                (self.root / 'aws_calls').unlink()
                (self.root / 'curl_calls').unlink()

    def test_boot_window_charges_outstanding_starts_across_requests(self):
        self.policy(32)
        self.place('new')
        self.env.update(INDEXUS_READY_RESERVE_DEFICIT='16', INDEXUS_BOOT_ALLOWANCE='1')
        self.assertIn('launched=1', self.run_hook().stderr)
        self.assertEqual(self.launched(), [1])
        self.assertIn('booting=1 launched=0', self.run_hook().stderr)
        self.env['INDEXUS_BOOT_ALLOWANCE'] = '2'
        self.assertIn('booting=1 launched=1', self.run_hook().stderr)

    def test_concurrent_hooks_share_one_boot_credit_below_the_machine_cap(self):
        self.policy(64)
        self.place('new')
        self.env.update(INDEXUS_READY_RESERVE_DEFICIT='16', INDEXUS_BOOT_ALLOWANCE='1')
        self.command('sleep', '#!/bin/sh\nexec /bin/sleep "$@"\n')
        with ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(lambda _: self.run_hook(), range(2)))
        self.assertEqual(sorted(r.stderr.split('launched=')[-1].strip() for r in results), ['0', '1'])
        self.assertEqual(len(list((self.root / 'booting.d').glob('*.boot'))), 1)
        self.assertEqual(self.launched(), [1])

    def test_named_target_does_not_wait_on_speculative_boots(self):
        self.policy(32)
        (self.root / 'booting.d' / '.calc.lock').mkdir(parents=True)
        self.place('new', 'http://host7')
        self.env.update(INDEXUS_READY_RESERVE_DEFICIT='10', INDEXUS_BOOT_ALLOWANCE='1')
        result = self.run_hook()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), 'local-33')
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if 'capacity calculation busy' in (self.root / 'machines.log').read_text():
                break
            time.sleep(.02)
        else:
            self.fail('the separate reserve check did not report its rejected reservation')
        self.assertEqual(self.spawned(), ['http://host7/spawn?prefer_near='])
        self.assertFalse((self.root / 'launches').exists())

    def test_concurrent_spare_reconciliation_does_not_survey_twice(self):
        with (self.root / 'reserve.lock').open('a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            # Run the real detached entry point while another survey holds its lock.
            r = subprocess.run(['python3', str(SCRIPT.with_name('boot_supply.py')), '--reconcile',
                                str(self.root / 'reserve.lock'), str(SCRIPT)], env=self.env)
            self.assertEqual(r.returncode, 0)
        self.assertFalse((self.root / 'aws_calls').exists())
        self.assertFalse((self.root / 'curl_calls').exists())

    def complete_aws_survey(self):
        self.env.update(INDEXUS_READY_RESERVE_DEFICIT='1',
                        INDEXUS_RESERVE_AGENTS=json.dumps([f'http://host{i}' for i in range(16)]))
        self.place('new')

    def test_unplaced_waiters_share_capacity_work_without_reporting_a_vnode(self):
        self.complete_aws_survey()
        with (self.root / 'reserve.lock').open('a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            result = self.run_hook()
            self.assertEqual(result.returncode, 75)
            self.assertEqual(result.stderr.strip().splitlines()[0], "INDEXUS_NO_RECEIVER")
            self.assertIn('fleet capacity reconciliation in progress', result.stderr)
            self.assertFalse((self.root / 'aws_calls').exists())
            self.assertFalse((self.root / 'curl_calls').exists())
        # When the reconciliation finishes a fresh request can supply the boot.
        self.policy(17)
        result = self.run_hook()
        self.assertIn('launched=1', result.stderr)
        self.assertEqual(self.launched(), [1])
        self.assertFalse((self.root / 'curl_calls').exists())
        self.assertIn('ec2 describe-instances', (self.root / 'aws_calls').read_text())

    def test_at_cap_target_does_not_share_another_waiters_decision(self):
        for reserve_known in (True, False):
            with self.subTest(reserve_known=reserve_known):
                self.complete_aws_survey()
                if not reserve_known:
                    del self.env['INDEXUS_READY_RESERVE_DEFICIT']
                self.place('new', cap_agent='http://host3')
                with (self.root / 'reserve.lock').open('a') as lock:
                    fcntl.flock(lock, fcntl.LOCK_EX)
                    result = self.run_hook()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.spawned(), ['http://host3/spawn?prefer_near='])
                self.assertEqual((self.root / 'curl_calls').read_text().splitlines(), self.spawned())
                (self.root / 'spawned').unlink()
                (self.root / 'curl_calls').unlink()

    def test_incomplete_survey_waits_without_repolling_or_speculative_boots(self):
        self.complete_aws_survey()
        del self.env['INDEXUS_READY_RESERVE_DEFICIT']
        self.policy(64)
        self.env['SPARE_RATIO'] = '300'
        with (self.root / 'reserve.lock').open('a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            result = self.run_hook()
            self.assertEqual(result.returncode, 75)
            self.assertIn('reconciliation in progress', result.stderr)
            self.assertFalse((self.root / 'aws_calls').exists())

        # EC2 sees an unreported recent host. Count it as supply once
        # even though neither a ready reserve nor an address was selected.
        (self.root / 'fleet').write_text('17')
        result = self.run_hook()
        self.assertEqual(result.returncode, 75)
        self.assertIn('booting=1 launched=0', result.stderr)
        self.assertFalse((self.root / 'launches').exists())
        self.assertIn('reserve=unknown', (self.root / 'machines.log').read_text())

        # After that host reports, an actual unplaced request can boot one.
        self.env['INDEXUS_RESERVE_AGENTS'] = json.dumps([f'http://host{i}' for i in range(17)])
        self.place('new', machines=17)
        result = self.run_hook()
        self.assertIn('booting=0 launched=1', result.stderr)
        self.assertEqual(self.launched(), [1])
        self.assertFalse((self.root / 'curl_calls').exists())

    def test_census_counts_the_union_of_surveyed_and_ec2_hosts(self):
        from boot_supply import count
        now = time.time()
        fresh = datetime.fromtimestamp(now, timezone.utc).isoformat()
        ec2 = [('both', fresh, 'running', ''), ('boot', fresh, 'pending', '')]
        agents = ['http://both:22100', 'http://agent-only:22100']
        self.assertEqual(count(ec2, {}, agents, now, 180), (3, 0, 1))

    def test_changed_inventory_keeps_resource_selected_target(self):
        (self.root / 'count').write_text('1 1')
        self.place('existing', 'http://host7', machines=15)
        result = self.run_hook()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.spawned(), ['http://host7/spawn?prefer_near='])
        self.assertIn('placed kind=existing', result.stderr)
        self.assertFalse((self.root / 'launches').exists())

    def test_partial_resource_facts_do_not_boot_spares_on_top_of_a_placement(self):
        self.policy(64)
        (self.root / 'count').write_text('1 1')
        self.env['SPARE_RATIO'] = '200'
        self.place('existing', 'http://host7')
        result = self.run_hook()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.spawned(), ['http://host7/spawn?prefer_near='])
        self.assertIn('deficit=0', (self.root / 'machines.log').read_text())

    def test_resource_refusal_does_not_fall_back_to_free_slots(self):
        (self.root / 'count').write_text('1 1')
        self.place('refuse', reason='no resource headroom')
        result = self.run_hook()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.spawned(), [])
        self.assertIn('placement refused', result.stderr)
        self.assertFalse((self.root / 'aws_calls').exists())
        self.assertFalse((self.root / 'curl_calls').exists())
        self.assertFalse((self.root / 'launches').exists())

    def test_waiting_for_a_new_machine_boots_one_and_places_nothing(self):
        (self.root / 'count').write_text('1 1')  # free slots, but none with headroom
        self.policy(64)
        self.place('new', cap_agent='http://host3', reason='no machine has headroom')
        result = self.run_hook()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('waiting for a new machine: no machine has headroom', result.stderr)
        self.assertEqual(self.spawned(), [])
        deadline = time.monotonic() + 3
        while not (self.root / 'launches').exists() and time.monotonic() < deadline:
            time.sleep(.01)
        launches = [json.loads(line) for line in (self.root / 'launches').read_text().splitlines()]
        self.assertEqual(len(launches), 1)
        self.assertEqual(launches[0][launches[0].index('--count')+1], '1')

    def test_waiting_for_a_machine_already_booting_launches_no_other(self):
        (self.root / 'count').write_text('1 1')
        self.policy(64)
        boot = self.root / 'booting.d'
        boot.mkdir()
        (boot / 'reserved.boot').write_text(str(int(time.time())))
        self.place('new', cap_agent='http://host3')
        result = self.run_hook()
        self.assertIn('waiting for a new machine', result.stderr)
        self.assertIn('booting=1 launched=0', result.stderr)
        self.assertEqual(self.spawned(), [])
        self.assertFalse((self.root / 'launches').exists())

    def launched(self):
        deadline = time.monotonic() + 3
        # The background fake creates the file before writing its first line.
        # Existence alone races that append when the machine is busy.
        path = self.root / 'launches'
        while time.monotonic() < deadline:
            try:
                rows = path.read_text().splitlines()
                if rows:
                    launches = [json.loads(line) for line in rows]
                    return [int(args[args.index('--count')+1]) for args in launches]
            except (FileNotFoundError, json.JSONDecodeError):
                pass
            time.sleep(.01)
        self.fail('background launch did not publish a complete record')

    def test_backlog_boots_beyond_the_machine_already_booting(self):
        # Three donors wait (floor 1 + pending 3 = 4, counted by the
        # provisioner): the one boot in flight covers one of them, not all.
        (self.root / 'count').write_text('1 1')
        self.policy(64)
        boot = self.root / 'booting.d'
        boot.mkdir()
        (boot / 'reserved.boot').write_text(str(int(time.time())))
        self.place('new')
        self.env.update(INDEXUS_READY_RESERVE_DEFICIT='4', INDEXUS_RESERVE_DETAIL='floor=1,pending=3,ready=0,warming=0')
        result = self.run_hook()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('waiting for a new machine', result.stderr)
        self.assertIn('booting=1 launched=3', result.stderr)
        self.assertEqual(self.launched(), [3])
        self.assertEqual(self.spawned(), [])
        self.assertIn('reserve=floor=1,pending=3,ready=0,warming=0 place=new', (self.root / 'machines.log').read_text())

    def test_tokens_deduplicate_visible_instances_and_unissued_reservations(self):
        from boot_supply import count
        now = time.time()
        fresh = datetime.fromtimestamp(now, timezone.utc).isoformat()
        stale = datetime.fromtimestamp(now - 181, timezone.utc).isoformat()
        instances = [('seen',fresh,'running','wave'), ('unseen',fresh,'pending','wave'),
                     ('stopped',fresh,'stopped','older'), ('stale',stale,'running','earlier')]
        # One of three reservations for wave has not reached EC2. One of
        # the two accepted instances is already included in the facts.
        self.assertEqual(count(instances, {'wave':3}, ['http://seen:22100'], now, 180),
                         (4, 1, 1))

    def test_ec2_hosts_missing_from_reserve_snapshot_cover_waiting_donors(self):
        # AWS: the provisioner sampled four agents, fifteen new EC2 machines
        # were accepted, all launch stamps for the wave have been cleared.
        self.policy(32)
        (self.root/'fleet').write_text('19')
        self.place('new',machines=4)
        self.env.update(INDEXUS_READY_RESERVE_DEFICIT='15',
                        INDEXUS_RESERVE_AGENTS=json.dumps([f'http://host{i}' for i in range(4)]))
        result=self.run_hook()
        self.assertIn('booting=15 launched=0',result.stderr)
        self.assertFalse((self.root/'launches').exists())

    def test_backlog_is_bounded_by_the_machine_cap(self):
        (self.root / 'count').write_text('1 1')
        self.policy(18)  # 16 machines up: room for two
        self.place('new')
        self.env['INDEXUS_READY_RESERVE_DEFICIT'] = '4'
        result = self.run_hook()
        self.assertIn('waiting for a new machine', result.stderr)
        self.assertEqual(self.launched(), [2])

    def test_reserve_covered_by_warming_machines_boots_nothing(self):
        # The provisioner counted this request and found it covered by
        # machines that registered and are settling: no boot is forced.
        (self.root / 'count').write_text('1 1')
        self.policy(64)
        self.place('new')
        self.env['INDEXUS_READY_RESERVE_DEFICIT'] = '0'
        result = self.run_hook()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('waiting for a new machine', result.stderr)
        self.assertIn('booting=0 launched=0', result.stderr)
        time.sleep(.2)
        self.assertFalse((self.root / 'launches').exists())
        self.assertEqual(self.spawned(), [])

    def test_backlog_at_machine_cap_does_not_wait_for_a_boot(self):
        (self.root / 'count').write_text('1 1')  # capacity 16, 16 machines
        self.place('new')
        self.env['INDEXUS_READY_RESERVE_DEFICIT'] = '3'
        result = self.run_hook()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('no free slot: capacity=16', result.stderr)
        self.assertFalse((self.root / 'launches').exists())

    def test_at_machine_cap_the_fallback_is_placed(self):
        (self.root / 'count').write_text('1 1')  # capacity 16, 16 machines: nothing can boot
        self.place('new', cap_agent='http://host3')
        result = self.run_hook()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.spawned(), ['http://host3/spawn?prefer_near='])
        self.assertIn('placed kind=cap agent=http://host3', result.stderr)
        self.assertIn('why=machine cap reached', (self.root / 'machines.log').read_text())
        self.assertFalse((self.root / 'launches').exists())

    def test_at_machine_cap_without_fallback_there_is_no_slot(self):
        self.place('new')
        result = self.run_hook()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('no free slot: capacity=16', result.stderr)
        self.assertEqual(self.spawned(), [])

    def test_facts_hook_prints_one_line_per_answering_agent(self):
        (self.root / 'facts').write_text(json.dumps(dict(running=1, free=1, cpu_pct=12.5, cpu_ok=True)))
        result = subprocess.run(['bash', str(SCRIPT.with_name('facts.sh'))], env=self.env, text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = [json.loads(line) for line in result.stdout.splitlines()]
        self.assertEqual([r['agent'] for r in rows], [f'http://host{i}' for i in range(16) if i != 1])
        self.assertEqual(rows[0]['facts'], dict(running=1, free=1, cpu_pct=12.5, cpu_ok=True))

    def test_busy_calculation_lock_fails_closed(self):
        self.policy(64)
        (self.root / 'booting.d' / '.calc.lock').mkdir(parents=True)
        result = self.run_hook()
        self.assertIn('capacity calculation busy', result.stderr)
        self.assertFalse((self.root / 'launches').exists())

    def test_unissued_reservations_prevent_overshoot(self):
        self.policy(17)
        boot = self.root / 'booting.d'
        boot.mkdir()
        (boot / 'reserved.boot').write_text(str(int(time.time())))
        result = self.run_hook()
        self.assertIn('booting=1 launched=0', result.stderr)
        self.assertFalse((self.root / 'launches').exists())

    def test_concurrent_spawns_cannot_reserve_the_last_machine_twice(self):
        self.policy(17)
        self.env['MOCK_ACCEPT'] = '1'
        with ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(lambda _: self.run_hook(), range(2)))
        deadline = time.monotonic() + 3
        while int((self.root / 'fleet').read_text()) != 17 and time.monotonic() < deadline:
            time.sleep(.01)
        self.assertEqual(int((self.root / 'fleet').read_text()), 17)
        self.assertEqual(len((self.root / 'launches').read_text().splitlines()), 1)
        self.assertTrue(all(r.returncode != 0 for r in results))

    def test_invalid_or_incompatible_policy_fails_closed(self):
        self.place('existing', 'http://host7')
        self.env['INDEXUS_READY_RESERVE_DEFICIT'] = '0'
        for content in ('invalid json', json.dumps(dict(max_machines=64, slots=8, reason='budget'))):
            (self.root / 'policy').write_text(content)
            result = self.run_hook()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('invalid capacity policy', result.stderr)
            self.assertEqual(self.spawned(), [])
            self.assertFalse((self.root / 'launches').exists())


if __name__ == '__main__':
    unittest.main()
