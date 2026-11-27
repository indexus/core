# Faults and hostile clients

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

What happens to acknowledged writes, to ownership and to the knowledge of the
network when nodes crash, restart, are cut off, or when clients misbehave?

This page first defines the scenarios (REM-101): what each injects, how, and
what green means. Results are added below each scenario as they are measured,
with the method of [protocol.md](protocol.md).

## Criteria common to every scenario

A scenario is green when all of these hold:

1. **No acknowledged write lost.** The audit reads back acknowledged keys
   (a sample, and every key of a collection whose total disagrees) after the
   network has settled.
2. **Never two writers for one zone.** No zone is writable on two nodes at any
   audit, and the commit-time check (the writer is the owner in the
   transaction that commits) refuses a write from a former owner.
3. **One knowledge version in bounded time.** After the fault ends, every live
   node agrees on one knowledge version within 3 gossip rounds (30 s), or within
   the time the topology change needs if the fault caused one.
4. **Bounded control.** Knowledge stays under 2 KB per vnode (sentinel
   `knowledge_bloat`) and no handoff stays open more than 2 minutes.
5. **Exact totals** once settled, per collection.

Availability during the fault is measured, not required: the share of offered
writes acknowledged, and how long until it returns to the level before the
fault.

## Node faults

| # | Scenario | Injection | Tool today | Extra measurement |
| --- | --- | --- | --- | --- |
| F1 | Crash and restart | SIGKILL 10% of the vnodes under load, restart on the same disk after 10 s | agent `POST /restart?slot=` (churn phase of `scenarios.sh`) | time until acked rate is back; writes refused meanwhile |
| F2 | Crash, no restart | SIGKILL one vnode and leave it down | agent `POST /kill?slot=` | time until its zones are writable elsewhere; stale routes (forwarded share) |
| F3 | Freeze | SIGSTOP a vnode for 30 s, then SIGCONT | by hand today (sentinel proof, palier 0) | a frozen owner must not acknowledge after it lost its zones |
| F4 | Partition | cut one vnode, then a half of the network, from the others for 60 s | **to build**: agent drops traffic to a set of peers | no write acknowledged by both sides for one zone; one version after the heal |
| F5 | Slow disk | add 50 ms to every fsync on one vnode | **to build**: fault hook in storage | the slow node's queue stays bounded; others unaffected |
| F6 | Crash during a handoff | kill the donor, then the receiver, at each step of freeze / stage / emit / install | **to build**: agent pause points (exist in tests: `TestReadsAndSingleWriterThroughHandoffAndLostMessages`) | no zone left fenced (REM-88); the transfer finishes or aborts |
| F7 | Joins under load | add vnodes one at a time at 2 000 writes/s per vnode | agent `POST /spawn` | handoff duration and MB/s; dip in acked rate (REM-87) |

## Hostile clients

| # | Scenario | Injection | Tool today | Expected |
| --- | --- | --- | --- | --- |
| C1 | Flood | 10× the green rate from one client | `experiment load -rate` | refusals (admission), no collapse of other clients' acked rate |
| C2 | Single hot key | every write on one zone | `-scenario hot` | the single-zone ceiling (Q8) is measured; other zones unaffected |
| C3 | Deep keys | ever longer shared prefixes | `-scenario deep` | splits stay bounded; knowledge bounded |
| C4 | Oversized payloads | payloads at and above the limit, metrics above 128 | **to build**: load option | refused by validation with 4xx, never retried forever |
| C5 | Invalid keys and tokens | bad locations, names, expired or forged tokens | **to build**: load option | 400 / 401, no state change |
| C6 | Replay | the same request id many times | **to build**: load option | applied once |

## Hostile or failing nodes

| # | Scenario | Injection | Expected |
| --- | --- | --- | --- |
| N1 | False zone claim | a node claims a zone it does not own | the claim is not accepted without a valid certificate; writes still go to the owner |
| N2 | Stale knowledge | a node restarts from an old knowledge version | it catches up; its stale routes are corrected by NotOwner replies |
| N3 | Invalid certificate | a region certificate with a bad signature | rejected; the network keeps the valid version |

These need a test build of the node that misbehaves on purpose; they come after
F1 to F7.

## Order

F1, F2, F7 and C1 to C3 use existing tools and run first, locally (defects),
then on AWS at 16 vnodes in passes. F3 to F6 and C4 to C6 need small additions
to the agent and the load; N1 to N3 a misbehaving node build.

## Results

None yet in the passes protocol. Earlier single runs: [p2p-findings-2026-11-15.md](p2p-findings-2026-11-15.md),
"Held" (0 acknowledged write lost under ramps, one hot zone and concurrent
readers) and the sentinel proof (a frozen node stopped the run in 11 s).
