# Brief: joining under load (2026-11-19)

**Statut : proposé.** Note de recherche.

For a model that will design and implement the fix. Read first:
`docs/research/split-latency-brief.md` (protocol, guarantees),
`docs/benchmarks/elasticity.md`, `internal/mesh/service.go` (handoff,
transferZones, Rebalance), `internal/autoscale/autoscale.go` (SCALE-1).

## What happened (AWS, run aws-dyn-load-20261119-172325)

4 vnodes capped at 1000 writes/s, spike of 3200 writes/s on one prefix for
120 s. The hot vnode asked for a peer on its write demand, as designed.

1. Its `POST /v1/scale` timed out after 15 s, while the spawn went on. The
   vnode asked again, 8 times, each time with the same `prefer_near`.
   12 vnodes were started in a few seconds, all near the same target.
2. All 12 joined through the same donor. A donor runs one handoff at a time
   (`handoffMu`); the first warm phase took 53 s under load. The others
   timed out ("join still unfinished", 75 times) or lost with ErrConflict.
3. No handoff reached its final phase before the spike ended. The cluster
   stayed at 4 active vnodes and acknowledged 59% of the spike.

Why SCALE-1 (one scale-up in flight per vnode) did not hold is not
verified. Also unexplained: "client sent an HTTP request to an HTTPS server"
on the donor, from two other hosts.

## The two problems, in order of depth

**A. Demand is not idempotent.** A retried ask creates a second vnode for
the same place. This is a control-loop defect, not a protocol one.

**B. Capacity arrives after the data.** A new vnode takes its first write
only when a handoff of about half the donor's zones has finished, and that
handoff is paid by the node that is already overloaded. Relief time grows
with the data held, so a short spike is always over first. Local runs show
the same thing (split of 18 000 zones: 21 to 27 s under load). This is the
real limit; A only made it visible.

## Critique of the first proposals

- "The provisioner waits until the new vnode has joined": it works, but it
  puts cluster state in the one central piece. The spirit of the protocol
  is that vnodes decide from what they know.
- "A busy donor answers busy at once": right, keep it. Alone it only turns
  a stampede into a queue: one split per 25 to 50 s.
- Neither touches B.

## Recommendations

1. **One target, one vnode.** The ask already names its target
   (`prefer_near`, from a validated plan). Make the spawn idempotent on
   that name, end to end: the provisioner and the agent answer "already
   starting" for a target they have seen, and the asking vnode keeps its
   in-flight mark through a timeout. No new state beyond a set of names.
2. **A vnode that cannot join yet is a spare, not an error.** The donor
   answers "busy" immediately; the newcomer stays out of the view, idle,
   and retries with backoff. Extra vnodes then cost nothing and form the
   warm pool that was wanted anyway. The donor serves one joiner at a time.
3. **Relief before data (the design change).** Give the newcomer the
   authority that costs no copy first, the data after:
   - zones that do not exist yet in its region can be written by the
     newcomer as soon as the split is certified: there is nothing to move
     and no writer to conflict with. A spike mostly creates new zones
     (about 300/s in the bench);
   - existing zones then flip one by one, hottest first, with the
     per-zone transfer that exists (XFER-KEYS-1), in the background.
   Reads already tolerate this since ad4b270: they are served from the
   source during a transfer and the reader keeps the newer copy.
   Open point to settle in the design: how the newcomer knows a zone
   "does not exist yet" at the donor without a round trip per write (a
   list or filter of the donor's zones sent once, or a point question
   cached per zone).
4. **Measure the right thing.** Add "seconds from the scale-up trigger to
   the first write applied by the new vnode" to the metrics and the
   sentinel. Target: a few seconds, independent of the items held.

## Invariants the design must keep (state them, then model-check)

- One writer per zone at every instant, across every crash cut
  (`internal/modelcheck`, `TestAuthorityAndCompleteReadsAtEveryDurableHandoffCut`).
- A zone that exists on the donor is written by the donor until its flip.
- Parent aggregates stay exact once converged; no global coordinator.
- A refused or lost message is retried or abandoned, never half-applied.

## Order of work

1 and 2 with the metric of 4: small, no protocol change; replay locally
(`ops/experiment/elastic_short.sh`, WRITE_CAP=300) then on AWS
(`MODE=dynamics WRITE_CAP=1000`, about 1.5 $, with the owner's approval).
3: write the design first (`docs/design/`), model-check, then implement.

## AWS run 3 (commit ee6f2cc, aws-dyn-load-20261119-183233)

Recommendations 1 and 2 hold on real machines: one joiner per target, it
started its handoff 1 s after the ask, the second joiner waited "busy"
instead of stampeding. But that one handoff took **295 s** (warm 0.8 s,
final phase 295 s under 3200 writes/s), longer than the spike (72 s): the
cluster stayed at 4 vnodes during it, acknowledging 66% of the spike.
This is problem B measured: relief waits for the data, and the data keeps
growing while the donor is the one under load. Recommendation 3 is next.

Also seen: `POST /v1/scale` takes more than 15 s on AWS (the node's call
times out while the spawn succeeds); harmless now (idempotent), but the
provisioner should answer at once and spawn in the background. Audit: 88
read timeouts on the overloaded genesis node, no refusal, no loss.
