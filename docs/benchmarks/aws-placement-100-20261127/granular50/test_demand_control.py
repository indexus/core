import json
from pathlib import Path
from tempfile import TemporaryDirectory
import unittest

from extract_request_episodes import failure_message, observation_window

from demand_control import cohort, early_forecast, replay, scores, simulate_burst


def case(donor, times, hook=None, write=None):
    return dict(donor=donor, attempts=times, hook=hook, write=write)


def ask(t, reason='', dt=.02, retry=0, n=1):
    return [t, reason, t + dt if reason else None, retry, n]


class DemandControlTest(unittest.TestCase):
    def test_429_wire_wait_is_classified_without_inventing_hook_cause(self):
        line = r'err="status 429: {\"error\":\"waiting_for_capacity\",\"retry_after_ms\":8786}\n"'
        self.assertEqual(failure_message(line), ('confirmed_wait', 8786))
        self.assertEqual(failure_message('net/http: context deadline exceeded'), ('timeout', 0))

    def test_missing_telemetry_censors_only_to_observed_injection(self):
        with TemporaryDirectory() as d:
            path = Path(d)
            rows = [dict(t=155, wall=1155, offered=2000, load_stale=False),
                    dict(t=400, wall=1400, load_stale=True)]
            (path / 'live.jsonl').write_text(''.join(json.dumps(r) + '\n' for r in rows))
            self.assertEqual(observation_window(path, 400), (1000, 155, 'dashboard'))
            raw = path / 'load.jsonl'
            raw.write_text('{"t":399.99995,"offered":16000}\n')
            self.assertEqual(observation_window(path, 400, raw), (1000, 399, 'load'))
            raw.write_text('{"t":400.0001,"offered":16000}\n')
            self.assertEqual(observation_window(path, 400, raw), (1000, 400, 'load'))

    def test_counts_applied_in_full_window(self):
        episodes = [case(1, [ask(2), ask(5), ask(45)], 3, 32),
                    case(2, [ask(3)], 4), case(3, [ask(70)], 70, 70.1)]
        rows, censored = cohort(episodes, 0, 90, 30)
        self.assertEqual(censored, 1)
        self.assertEqual(scores(rows), dict(eligible=2, useful=1, attempts=3,
                                           served_pct=50., calls_per_useful=3.))

    def test_one_donor_shares_inhibition_across_its_targets(self):
        first = case(1, [ask(0, 'coalesced'), ask(15)])
        second = case(1, [ask(1, 'wait_boot'), ask(3)])
        other = case(2, [ask(2)])
        self.assertEqual(replay([first, second, other], 'shipped')['sent'], 3)

    def test_no_delay_until_definite_single_target_reply(self):
        for reason, n in [('timeout', 1), ('other', 1), ('coalesced', 2)]:
            with self.subTest(reason=reason, n=n):
                rows = [ask(0, reason, 2, n=n), ask(1), ask(3)]
                self.assertEqual(replay([case(1, rows)], 'shipped')['sent'], 3)

    def test_suppressed_attempt_never_starts_a_second_delay(self):
        rows = [ask(0, 'capacity_lock'), ask(4, 'wait_boot'), ask(10.05)]
        self.assertEqual(replay([case(1, rows)], 'shipped')['sent'], 2)

    def test_api_does_not_delay_lone_waiter_and_spreads_followers(self):
        a = case(1, [ask(0, 'api_cd', .01, 2000), ask(2.02)])
        b = case(2, [ask(.5, 'api_cd', .01, 1500), ask(2.02)])
        self.assertEqual(replay([a], 'spread')['sent'], 2)
        self.assertEqual(replay([a, b], 'shipped')['sent'], 4)
        spread = replay([a, b], 'spread')
        self.assertEqual((spread['sent'], spread['followers']), (3, 1))

    def test_burst_serves_one_donor_per_slot_and_preserves_singleton(self):
        self.assertEqual(simulate_burst(1, False, 0), simulate_burst(1, True, 0))
        baseline = simulate_burst(32, False, 0)
        spread = simulate_burst(32, True, 0)
        self.assertLess(spread['attempts'], baseline['attempts'])
        for row in (baseline, spread):
            self.assertGreaterEqual(row['last'], 64)
            self.assertLessEqual(row['within_30'], 15)
            self.assertEqual(row['within_120'], 32)

    def test_forecast_never_uses_held_out_outcomes(self):
        train = case(1, [ask(0), ask(2)], 2, 3)
        held = case(2, [ask(300)], 301, 302)
        first = early_forecast([train, held], 120)
        held['write'] = None
        held['attempts'].append(ask(305))
        second = early_forecast([train, held], 120)
        self.assertNotEqual(first['observed'], second['observed'])
        self.assertEqual((second['predicted_useful'], second['predicted_calls']), (1, 2))


if __name__ == '__main__':
    unittest.main()
