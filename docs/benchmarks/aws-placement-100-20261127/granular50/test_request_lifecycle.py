import unittest
from request_lifecycle import OfferQueue, RequestLifecycle
from projection import CPU, Scenario, simulate


def fixed_response(placed, seconds):
    return {"completed": 1, "placed": int(placed), "response_samples": [[seconds, placed, 1]],
            "existing_completed": 1, "existing_placed": int(placed),
            "existing_samples": [[seconds, placed, 1]], "child_share_samples": [0.5]}


class RequestTests(unittest.TestCase):
    def test_unplaced_after_timeout(self):
        for ticket, allowed in [(False, 180), (True, 33)]:
            c = RequestLifecycle(fixed_response(False, 33), seed=1, ticket=ticket)
            c.submit(7, 0)
            for t in range(1, allowed):
                self.assertFalse(c.advance(t))
                c.submit(7, t)
            self.assertEqual(c.counts["requests"], 1)
            c.advance(allowed)
            c.submit(7, allowed)
            self.assertEqual(c.counts["requests"], 2)
            self.assertEqual(c.counts["client_timeouts"], 1)

    def test_acceptance_after_timeout_delivers_once(self):
        c = RequestLifecycle(fixed_response(True, 24), seed=2, ticket=True)
        c.submit(3, 0)
        for t in range(1, 24):
            self.assertFalse(c.advance(t))
            c.submit(3, t)
        self.assertEqual(c.advance(24), {3})
        self.assertFalse(c.advance(25))
        self.assertEqual(c.counts["requests"], 1)

    def test_failed_offer_keeps_same_intent_then_drops_on_relief(self):
        q = OfferQueue(fixed_response(False, 2), seed=1)
        q.update({0}, 0)
        q.reserve(0, 1, 2, 0)
        q.update({0}, 1)
        self.assertEqual(q.settle(2), [])
        q.update({0}, 2)
        self.assertEqual(q.waiting, {0: 0})
        self.assertEqual(q.counts["server_refusals"], 1)
        self.assertEqual(q.counts["requests"], 1)
        self.assertEqual(q.backlog(2), {"waiting_intents": 1, "reserved_intents": 0, "oldest_waiting_s": 2})
        q.update(set(), 3)
        self.assertFalse(q.waiting)

    def test_same_destination_reserved_once_until_copy_ends(self):
        # Two hot donors on distinct machines and one empty machine; make
        # either donor inadmissible as a receiver while the third stays idle.
        initial = [(0, 0.5), (1, 0.5), (2, 0)]
        case = Scenario("test", max_hosts=3, copy=3, reserved_cpu=True)
        cpu = CPU(0, 0.1, 0.2)
        q = OfferQueue(fixed_response(True, 4), seed=1)
        run = simulate(case, cpu, initial, [200] * 8, q)
        self.assertEqual(run[0]["reservation_attempts"], 1)
        for tick in run[::2]:
            blocked = sum(tick["waiting_" + cause] for cause in ("source_busy", "source_cpu", "no_receiver"))
            self.assertEqual(blocked, tick["waiting_intents"])
        self.assertEqual([r["splits_en_cours"] for r in run], [0, 0, 0, 0, 1, 1, 1, 0])
        self.assertEqual(run[-1]["vnodes"], 4)


if __name__ == "__main__":
    unittest.main()
