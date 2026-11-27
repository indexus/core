"""Exhaustive, bounded crash/replay check of the proposed vnode spawn contract.

One relief request, two possible agents, one crash-recoverable scheduler.
Only durable state is represented. Between any two transitions, the scheduler
or either agent can lose all volatile state. A delivered command or response
can always be replayed, delayed or dropped. An agent that never recovers can
leave the operation unresolved. This checks safety, not timing or fairness.
"""

from collections import deque
from dataclasses import dataclass, replace


@dataclass(frozen=True)
class State:
    accepted: bool = False
    cancelled: bool = False
    target: int | None = None
    # Bit i means the command may already have reached agent i.
    sent: int = 0
    # Each agent persists the request ID and its chosen local slot together.
    reserved: int = 0
    # External effects survive a lost reply. A second new slot is a failure.
    created: tuple[int, int] = (0, 0)
    completed: bool = False


def transitions(s, *, forget_target=False, repeat_start=False):
    if not s.accepted:
        yield "record intent", replace(s, accepted=True)
        return
    open_request = not (s.cancelled or s.completed)
    if open_request and s.target is None:
        yield "cancel before assignment", replace(s, cancelled=True)
        for i in (0, 1):
            yield f"reserve destination {i}", replace(s, target=i)
    elif open_request:
        i = s.target
        yield f"dispatch to {i}", replace(s, sent=s.sent | (1 << i))
        if forget_target:
            yield "restart loses destination", replace(s, target=None)
        if s.created[i]:
            yield f"reconcile observed instance {i}", replace(s, completed=True)
    # Old messages can still reach agents after the caller has finished.
    for i in (0, 1):
        bit = 1 << i
        if not s.sent & bit:
            continue
        if not s.reserved & bit:
            yield f"agent {i} records slot", replace(s, reserved=s.reserved | bit)
        elif s.created[i] == 0 or repeat_start:
            # In the safe contract create-or-discover addresses the same
            # durable instance identity even if its launch acknowledgement was lost.
            counts = list(s.created)
            counts[i] += 1
            yield f"agent {i} starts instance", replace(s, created=tuple(counts))


def explore(**fault):
    initial = State()
    previous = {initial: None}
    queue = deque([initial])
    completed_on = set()
    while queue:
        s = queue.popleft()
        if sum(s.created) > 1 or (s.cancelled and sum(s.created)):
            path = []
            while previous[s] is not None:
                s, action = previous[s]
                path.append(action)
            return len(previous), completed_on, list(reversed(path))
        if s.completed:
            completed_on.add(s.target)
        for action, successor in transitions(s, **fault):
            if successor not in previous:
                previous[successor] = s, action
                queue.append(successor)
    return len(previous), completed_on, []


if __name__ == "__main__":
    count, completed, violation = explore()
    assert not violation and completed == {0, 1}
    print(f"{count} reachable durable states, completion reachable on either agent.")
    for fault in ("forget_target", "repeat_start"):
        _, _, violation = explore(**{fault: True})
        assert violation, f"mutation {fault} escaped the check"
        print(f"{fault}: {' → '.join(violation)}")
