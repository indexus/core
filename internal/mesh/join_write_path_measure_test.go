package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// TestMeasuredJoinWritePath measures where client writes go while a vnode
// joins under load, in relay mode: clients write through an entry vnode (N1)
// that owns none of the moving keys; N0 gives N2 its cell. It counts, per
// write, the peer calls it took (entry→source→receiver is two), the full
// Knowledge bundles on the wire, and when the receiver first gets a write
// straight from the entry. In process: no disk fsync, no real network; a
// round trip and an optional full-bundle cost are added in the transport.
//
//	INDEXUS_JOIN_MEASURE=1 go test -run TestMeasuredJoinWritePath -v ./internal/mesh
//	INDEXUS_JOIN_RTT_US (default 1000), INDEXUS_JOIN_BUNDLE_MS (default 0):
//	the extra time a call carrying a full bundle takes (a large bundle on a
//	real link and its decoding), INDEXUS_JOIN_WORKERS (32), INDEXUS_JOIN_ZONES (512).
func TestMeasuredJoinWritePath(t *testing.T) {
	if os.Getenv("INDEXUS_JOIN_MEASURE") != "1" {
		t.Skip("set INDEXUS_JOIN_MEASURE=1")
	}
	integer := func(key string, fallback int) int {
		if v := os.Getenv(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				t.Fatalf("invalid %s", key)
			}
			return n
		}
		return fallback
	}
	rtt := time.Duration(integer("INDEXUS_JOIN_RTT_US", 1000)) * time.Microsecond
	bundle := time.Duration(integer("INDEXUS_JOIN_BUNDLE_MS", 0)) * time.Millisecond
	workers, zoneCount := integer("INDEXUS_JOIN_WORKERS", 32), integer("INDEXUS_JOIN_ZONES", 512)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	net, nodes := meshFixture(t, 3)
	source, entry, receiver := nodes[0], nodes[1], nodes[2]
	if err := entry.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := source.Sync(ctx, []string{entry.Self.Host}); err != nil {
		t.Fatal(err)
	}

	// Keys of N2's future cell that N0 holds now.
	const collection = "0000000000000001"
	view := tree.NewView(0)
	for _, n := range nodes {
		if err := view.Add(n.VNode.Name, n.VNode.ID); err != nil {
			t.Fatal(err)
		}
	}
	cell, err := tree.Cell(view, receiver.VNode.Name)
	if err != nil {
		t.Fatal(err)
	}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	var locations []string
	for i := 0; len(locations) < zoneCount && i < 64*64*64; i++ {
		loc := string([]byte{alphabet[i/4096%64], alphabet[i/64%64], alphabet[i%64]})
		z, err := tree.NewZoneKey(collection, loc)
		if err != nil {
			t.Fatal(err)
		}
		if !cell.Contains(z.ID) {
			continue
		}
		if h, err := source.Store.ZoneHolder(z); err != nil || h != source.Self {
			continue
		}
		locations = append(locations, loc)
	}
	if len(locations) < zoneCount {
		t.Fatalf("found %d zones of the receiver's cell held by the source", len(locations))
	}
	payload := json.RawMessage(`"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"`)
	write := func(id, loc string) (Response, error) {
		r := storage.Record{Collection: collection, Location: loc, ID: id, Metrics: []float64{1}, Payload: payload}
		// As the node's write API does (indexus meshWrite): compact replies.
		return entry.Handle(ctx, Request{Kind: "write", RequestID: id, Record: &r})
	}
	for i, loc := range locations {
		for j := range 4 {
			if _, err := write(fmt.Sprintf("seed-%d-%d", i, j), loc); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Transport: a round trip on every call, the bundle cost on full ones.
	var (
		measuring                     atomic.Bool
		fullBundles, receiverInstalls atomic.Int64
		writesAtReceiver              [4]atomic.Int64 // by Hops on arrival
		firstInstall, firstDirect     atomic.Int64    // unix ns
		firstRelayed                  atomic.Int64    // unix ns, entry→source→receiver
	)
	net.setBefore(func(r Request) error {
		time.Sleep(rtt / 2)
		if r.Membership != nil {
			if measuring.Load() {
				fullBundles.Add(1)
			}
			time.Sleep(bundle)
		}
		return nil
	})
	net.setAfter(func(r Request) error {
		time.Sleep(rtt / 2)
		return nil
	})
	receiverHost := receiver.Self.Host
	observe := &observingNet{network: net, observe: func(host string, r Request) {
		if host != receiverHost || !measuring.Load() {
			return
		}
		switch r.Kind {
		case "install-zone":
			receiverInstalls.Add(1)
			firstInstall.CompareAndSwap(0, time.Now().UnixNano())
		case "write":
			writesAtReceiver[min(r.Hops, 3)].Add(1)
			switch r.Hops {
			case 1:
				firstDirect.CompareAndSwap(0, time.Now().UnixNano())
			case 2:
				firstRelayed.CompareAndSwap(0, time.Now().UnixNano())
			}
		}
	}}
	for _, n := range nodes {
		n.Peers = observe
	}

	// Load: workers write through the entry while N2 joins, and after.
	type sample struct {
		at, latency time.Duration
		appliedBy   string
	}
	var (
		mu       sync.Mutex
		samples  []sample
		refusals atomic.Int64
		stop     = make(chan struct{})
		wg       sync.WaitGroup
	)
	measuring.Store(true)
	began := time.Now()
	for w := range workers {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				loc := locations[(w*7919+i*104729)%len(locations)]
				id := fmt.Sprintf("live-%d-%d", w, i)
				started := time.Now()
				for {
					out, err := write(id, loc)
					if err == nil {
						mu.Lock()
						samples = append(samples, sample{at: started.Sub(began), latency: time.Since(started), appliedBy: out.AppliedBy.VNode})
						mu.Unlock()
						break
					}
					if ctx.Err() != nil {
						return
					}
					if !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrNotOwner) {
						t.Errorf("write %s: %v", id, err)
						return
					}
					refusals.Add(1)
					time.Sleep(time.Millisecond)
				}
			}
		})
	}
	time.Sleep(200 * time.Millisecond)
	joinStart := time.Now()
	if err := receiver.Join(ctx); err != nil {
		close(stop)
		wg.Wait()
		t.Fatal(err)
	}
	joined := time.Since(joinStart)
	time.Sleep(time.Second) // after the join: does the entry reach N2 directly?
	close(stop)
	wg.Wait()
	measuring.Store(false)

	percentile := func(values []time.Duration, p int) float64 {
		if len(values) == 0 {
			return 0
		}
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		return float64(values[(p*len(values)+99)/100-1].Microseconds()) / 1000
	}
	joinFrom, joinTo := joinStart.Sub(began), joinStart.Sub(began)+joined
	var during, after []time.Duration
	atReceiver := 0
	for _, s := range samples {
		if s.appliedBy == receiver.VNode.Name {
			atReceiver++
		}
		switch {
		case s.at >= joinFrom && s.at < joinTo:
			during = append(during, s.latency)
		case s.at >= joinTo:
			after = append(after, s.latency)
		}
	}
	since := func(at int64) float64 {
		if at == 0 {
			return -1
		}
		return float64(time.Unix(0, at).Sub(joinStart).Microseconds()) / 1000
	}
	direct, twoHops := writesAtReceiver[1].Load(), writesAtReceiver[2].Load()
	result := map[string]any{
		"rtt_us": rtt.Microseconds(), "bundle_ms": bundle.Milliseconds(), "workers": workers, "zones": len(locations),
		"join_ms":                  float64(joined.Microseconds()) / 1000,
		"acks":                     len(samples),
		"acks_by_receiver":         atReceiver,
		"refusals_retried":         refusals.Load(),
		"receiver_installs":        receiverInstalls.Load(),
		"full_bundles":             fullBundles.Load(),
		"writes_at_receiver_1hop":  direct,
		"writes_at_receiver_2hops": twoHops,
		"first_install_ms":         since(firstInstall.Load()),
		"first_relayed_write_ms":   since(firstRelayed.Load()),
		"first_direct_write_ms":    since(firstDirect.Load()),
		"during_join_p50_ms":       percentile(during, 50),
		"during_join_p99_ms":       percentile(during, 99),
		"during_join_max_ms":       percentile(during, 100),
		"after_join_p50_ms":        percentile(after, 50),
		"after_join_p99_ms":        percentile(after, 99),
	}
	raw, _ := json.Marshal(result)
	t.Logf("JOIN_WRITE_PATH %s", raw)
}

// observingNet reports each peer call before delivering it.
type observingNet struct {
	*network
	observe func(host string, r Request)
}

func (o *observingNet) Exchange(ctx context.Context, host string, r Request) (Response, error) {
	o.observe(host, r)
	return o.network.Exchange(ctx, host, r)
}
