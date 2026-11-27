"""Stochastic scale requests with a single outstanding operation per donor."""
from collections import Counter
from dataclasses import dataclass
from random import Random


@dataclass(frozen=True)
class Request:
    started: int
    completed: int
    placed: bool


class RequestLifecycle:
    offers_first = False

    def __init__(self, training, seed, ticket=False):
        self.ticket = ticket
        self.random = Random(seed)
        self.placed_probability = training["placed"] / training["completed"]
        self.durations = {placed: [] for placed in (True, False)}
        for seconds, placed, count in training["response_samples"]:
            self.durations[placed].extend([seconds] * count)
        self.fractions = training["child_share_samples"]
        self.pending = {}
        self.blocked_until = {}
        self.counts = Counter()

    def submit(self, vnode, now):
        if vnode in self.pending or now < self.blocked_until.get(vnode, 0):
            return
        placed = self.random.random() < self.placed_probability
        delay = self.random.choice(self.durations[placed])
        self.pending[vnode] = Request(now, now + delay, placed)
        self.counts["requests"] += 1

    def advance(self, now):
        """Advance server operations even when the caller's deadline has passed."""
        placements = set()
        for vnode, request in list(self.pending.items()):
            if request.completed - request.started > 15 and now == request.started + 15:
                self.counts["client_timeouts"] += 1
            if now < request.completed:
                continue
            del self.pending[vnode]
            if request.placed:
                placements.add(vnode)
                continue
            self.counts["server_refusals"] += 1
            if not self.ticket and request.completed - request.started > 15:
                self.blocked_until[vnode] = request.started + 180
                self.counts["ambiguous_refusals"] += 1
        return placements


class OfferQueue:
    """An intent survives a failed reservation of one particular destination."""
    offers_first = True

    def __init__(self, training, seed):
        self.random = Random(seed)
        self.fractions = training["child_share_samples"]
        self.placed_probability = training["existing_placed"] / training["existing_completed"]
        self.durations = {placed: [] for placed in (True, False)}
        for seconds, placed, count in training["existing_samples"]:
            self.durations[placed].extend([seconds] * count)
        self.waiting = {}
        self.reservations = {}
        self.counts = Counter()

    def update(self, hot, now):
        # With changing traffic an unreserved intent expires as soon as its
        # donor is no longer hot. An in-flight reservation always settles.
        for vnode in self.waiting.keys() - hot - self.reservations.keys():
            del self.waiting[vnode]
        for vnode in sorted(hot - self.waiting.keys()):
            self.waiting[vnode] = now
            self.counts["requests"] += 1

    def backlog(self, now):
        waiting = self.waiting.keys() - self.reservations.keys()
        return {"waiting_intents": len(waiting), "reserved_intents": len(self.reservations),
                "oldest_waiting_s": max((now - self.waiting[v] for v in waiting), default=0)}

    def reserve(self, vnode, source, destination, now):
        placed = self.random.random() < self.placed_probability
        delay = self.random.choice(self.durations[placed])
        self.reservations[vnode] = (now + delay, source, destination, placed)
        self.counts["reservation_attempts"] += 1

    def settle(self, now):
        starts = []
        for vnode, (complete, _, destination, placed) in list(self.reservations.items()):
            if now < complete:
                continue
            del self.reservations[vnode]
            if placed:
                starts.append((vnode, destination))
            else:
                self.counts["server_refusals"] += 1
        return starts
