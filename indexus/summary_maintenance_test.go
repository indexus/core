package indexus

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/summary"
)

func TestBoundaryDiscoveryRepairsUnseenChildWhenPushesAreLost(t *testing.T) {
	t.Parallel()
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	ctx := context.Background()
	if err := f.nodes[1].JoinCausalCluster(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.services[0].Sync(ctx, []string{f.services[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	for _, n := range f.nodes {
		if err := n.meshRefreshView(); err != nil {
			t.Fatal(err)
		}
	}
	const collection = "0000000000000001"
	root, _ := NewZoneKey(collection, "@")
	owner, _ := f.nodes[0].mesh.summary.View().OwnerName(root.ID)
	parent, child := f.nodes[0], f.nodes[1]
	if parent.Name() != owner {
		parent, child = child, parent
	}
	location := ""
	for _, candidate := range ChildCandidates(BASE64, "@") {
		z, _ := NewZoneKey(collection, candidate)
		if name, _ := child.mesh.summary.View().OwnerName(z.ID); name == child.Name() {
			location = candidate
			break
		}
	}
	if location == "" {
		t.Fatal("missing remote child")
	}
	// A discovery poll must repair even a child the parent has never retained.
	// Deliberately lose every push; do not feed a known-child list to the poll.
	f.net.before = func(request mesh.Request) error {
		if request.Kind == mesh.KindSummaryContribute {
			return fmt.Errorf("injected lost push")
		}
		return nil
	}
	t.Cleanup(func() {
		for _, n := range f.nodes {
			n.mesh.summary.Mu.Lock()
			n.mesh.summary.StopFlushLocked()
			n.mesh.summary.Mu.Unlock()
		}
	})
	for i := 0; i < 3; i++ {
		record := storage.Record{Collection: collection, Location: location, ID: fmt.Sprint(i), Metrics: []float64{7}}
		if _, err := child.MeshData(ctx, mesh.Request{Kind: "write", RequestID: fmt.Sprint(i), Record: &record}); err != nil {
			t.Fatal(err)
		}
	}
	if got := parent.mesh.summary.Owned(collection, "@").Count(); got != 0 {
		t.Fatalf("push was not lost: %d", got)
	}
	if err := parent.mesh.summary.PollRemoteHosts(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := parent.mesh.summary.Owned(collection, "@"); got.Count() != 3 || got.Metric(0) != 21 {
		t.Fatalf("discovery did not repair root: %v", got)
	}
}

// An idle node polls its peers' boundaries less and less often; a write or an
// accepted contribution brings the poll back to its short cadence.
func TestBoundaryPollBacksOffWhileIdle(t *testing.T) {
	t.Parallel()
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	ctx := context.Background()
	if err := f.nodes[1].JoinCausalCluster(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.services[0].Sync(ctx, []string{f.services[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	polls := 0
	f.net.before = func(request mesh.Request) error {
		if request.Kind == mesh.KindSummaryGet {
			polls++
		}
		return nil
	}
	n := f.nodes[0]
	// The authority change keeps the short cadence for a few ticks.
	for i := 0; i <= summary.FullTicksAfterChange; i++ {
		if err := n.mesh.summary.Reconcile(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	elapse := func() {
		n.mesh.summary.Mu.Lock()
		n.mesh.summary.LastReconcile = time.Now().Add(-n.mesh.summary.PollEvery - time.Millisecond)
		n.mesh.summary.Mu.Unlock()
	}
	cadence := func() time.Duration {
		n.mesh.summary.Mu.Lock()
		defer n.mesh.summary.Mu.Unlock()
		return n.mesh.summary.PollEvery
	}
	start := cadence()
	for i := 0; i < 8; i++ {
		elapse()
		if err := n.mesh.summary.Reconcile(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := cadence(); got != summary.ReconcileIdleMax || start >= got {
		t.Fatalf("idle cadence %v after starting at %v, want %v", got, start, summary.ReconcileIdleMax)
	}
	before := polls
	reported, ok := n.SummaryMetrics()["poll_calls"].(int64)
	if !ok {
		t.Fatal("missing poll_calls in summary metrics")
	}
	if err := n.mesh.summary.Reconcile(ctx, false); err != nil {
		t.Fatal(err)
	}
	if polls != before {
		t.Fatal("polled again before the idle cadence elapsed")
	}
	n.mesh.lastWrite.Store(time.Now().UnixNano())
	n.mesh.summary.Mu.Lock()
	n.mesh.summary.LastReconcile = time.Now().Add(-summary.ReconcileEvery - time.Millisecond)
	n.mesh.summary.Mu.Unlock()
	if err := n.mesh.summary.Reconcile(ctx, false); err != nil {
		t.Fatal(err)
	}
	if polls == before {
		t.Fatal("a write did not bring the poll back to its short cadence")
	}
	if got := n.SummaryMetrics()["poll_calls"]; got != reported+int64(polls-before) {
		t.Fatalf("reported %v total polls, want %d", got, reported+int64(polls-before))
	}
}

// A paged boundary poll returns exactly the unpaged export, in order, with no
// page above its limit.
func TestPagedBoundaryExportEqualsWholeExport(t *testing.T) {
	t.Parallel()
	descriptors := []VNodeDescriptor{{ID: ID{16}}, {ID: ID{144}}}
	for i := range descriptors {
		descriptors[i].Name = EncodeName(descriptors[i].ID)
	}
	f := newReadPhaseFixture(t, descriptors, false, false)
	ctx := context.Background()
	if err := f.nodes[1].JoinCausalCluster(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.services[0].Sync(ctx, []string{f.services[1].Self.Host}); err != nil {
		t.Fatal(err)
	}
	for _, n := range f.nodes {
		if err := n.meshRefreshView(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, n := range f.nodes {
			n.mesh.summary.Mu.Lock()
			n.mesh.summary.StopFlushLocked()
			n.mesh.summary.Mu.Unlock()
		}
	})
	const collection = "0000000000000001"
	view := f.nodes[0].mesh.summary.View()
	// Depth-1 zones of one node below a root the other node owns.
	boundaries := func(child, parent *Node) []string {
		root, _ := NewZoneKey(collection, "@")
		if owner, _ := view.OwnerName(root.ID); owner != parent.Name() {
			return nil
		}
		var out []string
		for _, a := range ChildCandidates(BASE64, "@") {
			z, _ := NewZoneKey(collection, a)
			if owner, _ := view.OwnerName(z.ID); owner == child.Name() {
				out = append(out, a)
			}
		}
		return out
	}
	child, parent := f.nodes[0], f.nodes[1]
	locations := boundaries(child, parent)
	if other := boundaries(parent, child); len(other) > len(locations) {
		child, parent, locations = parent, child, other
	}
	written := 0
	for _, b := range locations {
		record := storage.Record{Collection: collection, Location: b, ID: "r", Metrics: []float64{1}}
		if _, err := child.MeshData(ctx, mesh.Request{Kind: "write", RequestID: b, Record: &record}); err != nil {
			t.Fatal(err)
		}
		written++
	}
	if written < 20 {
		t.Fatalf("fixture: only %d boundaries", written)
	}
	whole := child.mesh.summary.Export("", nil, parent.Name())
	if len(whole) < written {
		t.Fatalf("whole export has %d of %d boundaries", len(whole), written)
	}
	var paged []mesh.Contribution
	page := &mesh.SummaryPage{Limit: 7}
	for {
		out, more := child.mesh.summary.ExportPage("", nil, parent.Name(), page)
		if len(out) > page.Limit {
			t.Fatalf("page of %d above limit %d", len(out), page.Limit)
		}
		paged = append(paged, out...)
		if !more {
			break
		}
		last := out[len(out)-1]
		page = &mesh.SummaryPage{Collection: last.Collection, Location: last.Location, Limit: 7}
	}
	if len(paged) != len(whole) {
		t.Fatalf("paged %d, whole %d", len(paged), len(whole))
	}
	for i := range whole {
		if paged[i].Collection != whole[i].Collection || paged[i].Location != whole[i].Location || paged[i].Count != whole[i].Count {
			t.Fatalf("contribution %d: paged %+v, whole %+v", i, paged[i], whole[i])
		}
	}
	if err := parent.mesh.summary.PollHost(context.Background(), f.services[0].Self.Host); err != nil {
		t.Fatal(err)
	}
}
