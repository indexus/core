package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// work is the transfer work counters at one moment.
type work [9]int64

var workNames = [9]string{"rows put", "rows deleted", "commits", "point reads", "scans", "JSON record decodes", "log decodes", "log encodes", "seals"}

func workNow() work {
	return work{rowPuts.Load(), rowDeletes.Load(), rowCommits.Load(), pointReads.Load(), rangeScans.Load(), recordDecodes.Load(), logDecodes.Load(), logEncodes.Load(), sealsComputed.Load()}
}

func (w work) sub(o work) work {
	for i := range w {
		w[i] -= o[i]
	}
	return w
}

// lotLetters name the zones of a lot: one location character each.
const lotLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// lotWork is the work of each step of one lot's transfer, by side.
type lotWork struct {
	steps []string
	work  map[string]work
}

func (l *lotWork) step(t *testing.T, name string, fn func() error) {
	t.Helper()
	before := workNow()
	if err := fn(); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	l.steps = append(l.steps, name)
	l.work[name] = workNow().sub(before)
}

// transferOneLot moves a region of zones one-record zones from one store to
// another as the mesh does for one lot (copyLot, cutoverLot, the region
// install, the collection) and returns the work of each step. With preload
// false the receiver has no warm copy: the install carries the snapshots.
func transferOneLot(t *testing.T, zones int, preload bool) lotWork {
	source, target, genesis := ticketStores(t)
	keys := make([]tree.ZoneKey, zones)
	for i := range keys {
		r := Record{Collection: "Demo000000000000", Location: "a" + string(lotLetters[i]), ID: "one", Metrics: []float64{1}, Payload: []byte(`"payload"`)}
		if _, err := source.Write(fmt.Sprint("w", i), r); err != nil {
			t.Fatal(err)
		}
		keys[i], _ = recordZone(r)
	}
	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	id := op.Cession.ID
	out := lotWork{work: map[string]work{}}
	var snaps []Snapshot
	var seals []ZoneSeal
	var err error
	if preload {
		out.step(t, "source copy", func() error {
			snaps, err = source.Snapshots(context.Background(), keys, 0)
			return err
		})
		out.step(t, "receiver preload", func() error {
			offer := op
			offer.Snapshots = snaps
			_, err := target.PreloadCausal(offer)
			return err
		})
	}
	var proofs []causal.ZoneTransfer
	out.step(t, "source fence", func() error {
		snaps, seals, proofs, err = source.FenceZones(id, keys)
		return err
	})
	out.step(t, "receiver mount", func() error {
		if preload {
			return target.MountZones(id, source.node, nil, seals, nil, proofs)
		}
		return target.MountZones(id, source.node, nil, nil, snaps, proofs)
	})
	out.step(t, "source ack", func() error { return source.AckZoneInstalls(id, keys) })
	var offer CausalTransfer
	out.step(t, "source region seal", func() error {
		if _, err := source.ForceFreezeRegion(id); err != nil {
			return err
		}
		offer, err = source.SealCausalRegion(id)
		return err
	})
	out.step(t, "receiver region stage", func() error { return target.StageCausal(offer) })
	cession := op.Cession
	out.step(t, "source region emit", func() error {
		cession, err = source.EmitCausal(id)
		return err
	})
	out.step(t, "receiver region install", func() error { return target.InstallCausal(cession) })
	out.step(t, "source complete", func() error { return source.CompleteCausal(id) })
	out.step(t, "receiver collect", func() error { return target.CollectTransferred(id) })
	out.step(t, "source collect", func() error { return source.CollectTransferred(id) })
	for i, z := range keys {
		r := Record{Collection: z.Collection, Location: z.Location, ID: "after"}
		if _, err := target.Write(fmt.Sprint("after", i), r); err != nil {
			t.Fatalf("the receiver does not write zone %d after the transfer: %v", i, err)
		}
		snap, err := target.Snapshot(z)
		if err != nil || len(snap.Records) != 2 {
			t.Fatalf("zone %d holds %d records after the transfer, want 2 (%v)", i, len(snap.Records), err)
		}
	}
	return out
}

// lotSmall and lotLarge are the two lot sizes the table is drawn from.
const lotSmall, lotLarge = 8, 24

// lotWorkTable is the work of each step for two lots of different sizes.
// A count linear in the zones of the lot is printed as "z/zone + l": z per
// zone and l per lot. The others (scans, which read in pages that grow) are
// printed as the two raw counts.
func lotWorkTable(t *testing.T, title string, preload bool) (small, large lotWork) {
	small, large = transferOneLot(t, lotSmall, preload), transferOneLot(t, lotLarge, preload)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n| step |", title)
	for _, name := range workNames {
		fmt.Fprintf(&b, " %s |", name)
	}
	b.WriteString("\n|---|" + strings.Repeat("---|", len(workNames)) + "\n")
	for _, step := range small.steps {
		fmt.Fprintf(&b, "| %s |", step)
		for i := range workNames {
			z, l, linear := perZone(small.work[step][i], large.work[step][i])
			if linear {
				fmt.Fprintf(&b, " %d/zone + %d |", z, l)
			} else {
				fmt.Fprintf(&b, " %d ; %d |", small.work[step][i], large.work[step][i])
			}
		}
		b.WriteString("\n")
	}
	t.Log(b.String())
	return small, large
}

// perZone splits two counts, of a lot of lotSmall zones and of one of
// lotLarge, into a part per zone and a part per lot, when they are linear.
func perZone(small, large int64) (zone, lot int64, linear bool) {
	d := large - small
	zone = d / (lotLarge - lotSmall)
	return zone, small - lotSmall*zone, d%(lotLarge-lotSmall) == 0 && small >= lotSmall*zone
}

// TestLotTransferWork counts what the transfer of one lot of one-record
// zones costs each side, per zone and per lot: rows written and deleted,
// commits, reads, decodes and seals. Run it with -v for the table.
func TestLotTransferWork(t *testing.T) {
	small, large := lotWorkTable(t, "warm copy, then mount by seals; cells for lots of 8 and 24 zones", true)
	// A mount decodes each staged record once, to certify it: the install
	// writes what the certification decoded.
	if z, l, linear := perZone(small.work["receiver mount"][5], large.work["receiver mount"][5]); !linear || z != 1 || l != 0 {
		t.Errorf("the mount decoded %d and %d staged records for %d and %d one-record zones, want one each", small.work["receiver mount"][5], large.work["receiver mount"][5], lotSmall, lotLarge)
	}
	lotWorkTable(t, "no warm copy: the install carries the snapshots", false)
}
