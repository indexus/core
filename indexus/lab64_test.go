// This opt-in lab executes 64 node facades, mesh services and independent stores
// over loopback HTTP/JSON. Each pair shares a simulated fsync device; this is
// a contention/fault laboratory, not a substitute for 32 physical EC2 hosts.
package indexus

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/adapter/meshhttp"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

type device struct {
	gate      sync.Mutex
	delay     time.Duration
	syncs     atomic.Int64
	waitNS    atomic.Int64
	holdNS    atomic.Int64
	slowUntil atomic.Int64
	slowDelay atomic.Int64
}
type disk struct {
	zonelog.FS
	d *device
}
type diskFile struct {
	zonelog.File
	d *device
}

func (d disk) Create(n string) (zonelog.File, error) {
	f, e := d.FS.Create(n)
	if e != nil {
		return nil, e
	}
	return diskFile{f, d.d}, nil
}
func (d disk) Open(n string) (zonelog.File, error) {
	f, e := d.FS.Open(n)
	if e != nil {
		return nil, e
	}
	return diskFile{f, d.d}, nil
}
func (d *device) sync(fn func() error) error {
	at := time.Now()
	d.gate.Lock()
	d.waitNS.Add(int64(time.Since(at)))
	at = time.Now()
	delay := d.delay
	if time.Now().UnixNano() < d.slowUntil.Load() {
		delay += time.Duration(d.slowDelay.Load())
	}
	time.Sleep(delay)
	err := fn()
	d.holdNS.Add(int64(time.Since(at)))
	d.syncs.Add(1)
	d.gate.Unlock()
	return err
}
func (d disk) Sync() error     { return d.d.sync(d.FS.Sync) }
func (f diskFile) Sync() error { return f.d.sync(f.File.Sync) }

type journal struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (j *journal) event(v any) { j.mu.Lock(); defer j.mu.Unlock(); _ = j.enc.Encode(v) }

type link struct {
	base   mesh.Transport
	faults bool
	delay  time.Duration
	lost   sync.Map
	log    *journal
	calls  atomic.Int64
	failed atomic.Int64
}

func (l *link) Exchange(ctx context.Context, host string, r mesh.Request) (mesh.Response, error) {
	at := time.Now()
	timer := time.NewTimer(l.delay)
	select {
	case <-timer.C:
	case <-ctx.Done():
		timer.Stop()
		return mesh.Response{}, ctx.Err()
	}
	out, err := l.base.Exchange(ctx, host, r)
	l.calls.Add(1)
	if l.faults && r.Kind == "install-zone" && err == nil {
		if _, old := l.lost.LoadOrStore(r.Operation, true); !old {
			err = errors.New("lab: install ACK lost after durable commit")
		}
	}
	if err != nil {
		l.failed.Add(1)
	}
	if r.Kind != "write" && (err != nil || time.Since(at) > time.Second) {
		l.log.event(map[string]any{"event": "rpc", "kind": r.Kind, "target": host, "op": r.Operation, "ms": time.Since(at).Milliseconds(), "error": fmt.Sprint(err)})
	}
	return out, err
}
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil {
			panic(e)
		}
		return n
	}
	return def
}
func Test64NodeDurableHTTPRecovery(t *testing.T) {
	if os.Getenv("INDEXUS_LAB64") != "1" {
		t.Skip("opt-in: INDEXUS_LAB64=1; see docs/benchmarks/transfer-recovery-20261124.md")
	}
	if os.Getenv("INDEXUS_BENCH_NO_USAGE_SCAN") == "" && os.Getenv("INDEXUS_LAB_USAGE_SCAN") != "1" {
		t.Fatal("set INDEXUS_BENCH_NO_USAGE_SCAN=1 to exercise production incremental accounting; or explicitly set INDEXUS_LAB_USAGE_SCAN=1 for expensive differential correctness scans")
	}
	count := envInt("INDEXUS_LAB_NODES", 64)
	if count < 4 || count > 64 || count&(count-1) != 0 {
		t.Fatal("INDEXUS_LAB_NODES must be a power of two from 4 to 64")
	}
	faults := os.Getenv("INDEXUS_LAB_FAULTS") != "0"
	stacks := os.Getenv("INDEXUS_LAB_STACKS") != "0"
	traceWrites := os.Getenv("INDEXUS_LAB_WRITE_TRACE") == "1"
	out := os.Getenv("INDEXUS_LAB_OUT")
	if out == "" {
		out = t.TempDir()
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(out, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	log := &journal{enc: json.NewEncoder(logFile)}
	syncDelay := time.Duration(envInt("INDEXUS_LAB_SYNC_MS", 1)) * time.Millisecond
	linkDelay := time.Duration(envInt("INDEXUS_LAB_RTT_MS", 1)) * time.Millisecond
	fullNodes := os.Getenv("INDEXUS_LAB_MESH_ONLY") != "1"
	maintenance := os.Getenv("INDEXUS_LAB_MAINTENANCE") != "0"
	direct := os.Getenv("INDEXUS_LAB_DIRECT") == "1"
	uniqueZones := os.Getenv("INDEXUS_LAB_UNIQUE_ZONES") == "1"
	baseRate := envInt("INDEXUS_LAB_BASE_RATE", 2000)
	requestTimeout := time.Duration(envInt("INDEXUS_LAB_REQUEST_TIMEOUT_MS", 30000)) * time.Millisecond
	writeAttempts := envInt("INDEXUS_LAB_WRITE_ATTEMPTS", 5)
	tailRate := envInt("INDEXUS_LAB_TAIL_RATE", 0)
	seedZones := envInt("INDEXUS_LAB_SEED_ZONES", 0)
	storeMemoryMiB := envInt("INDEXUS_LAB_STORE_MEMORY_MIB", 2)
	if storeMemoryMiB < 1 {
		t.Fatal("INDEXUS_LAB_STORE_MEMORY_MIB must be positive")
	}
	storeMemory := storeMemoryMiB << 20
	phaseTime := time.Duration(envInt("INDEXUS_LAB_PHASE_SECONDS", 2)) * time.Second
	timeout := time.Duration(envInt("INDEXUS_LAB_TIMEOUT_SECONDS", 240)) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	transport := &link{base: meshhttp.NewClient(), delay: linkDelay, log: log, faults: faults}
	nodes := make([]*mesh.Service, count)
	facades := make([]*Node, count)
	firstInstall := make([]atomic.Bool, count)
	gates := make([]sync.RWMutex, count)
	blockedUntil := make([]atomic.Int64, count)
	servers := make([]*httptest.Server, count)
	devices := make([]*device, count/2)
	for i := range devices {
		devices[i] = &device{delay: syncDelay}
	}
	for i := range count {
		v, reversed := i, 0
		for range 6 {
			reversed = reversed*2 + v%2
			v /= 2
		}
		id := tree.ID{byte(reversed*4 + 1), 1}
		name := BASE64.Encode(id[:])
		path := filepath.Join(out, fmt.Sprintf("lab-%02d", i), "store")
		dir := storage.RecordsDir(path)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		store, err := storage.OpenWithOptions(path, name, "lab", 1<<30, storage.Options{FS: disk{zonelog.Dir(dir), devices[i/2]}, BatchWrites: true, BatchSize: 128, BatchDelay: time.Millisecond, MemoryBytes: storeMemory})
		if err != nil {
			t.Fatal(err)
		}
		s := &mesh.Service{Store: store, Peers: transport}
		nodes[i] = s
		srv := httptest.NewServer(&meshhttp.Handler{Mesh: func(ctx context.Context, r mesh.Request) (mesh.Response, error) {
			gates[i].RLock()
			defer gates[i].RUnlock()
			if time.Now().UnixNano() < blockedUntil[i].Load() {
				return mesh.Response{}, errors.New("lab: bounded receiver partition")
			}
			var out mesh.Response
			var err error
			if fullNodes {
				out, err = facades[i].MeshData(ctx, r)
			} else {
				out, err = nodes[i].Handle(ctx, r)
			}
			if r.Kind == "install-zone" && err == nil && firstInstall[i].CompareAndSwap(false, true) {
				log.event(map[string]any{"event": "first-zone-installed", "node": i, "unix_ns": time.Now().UnixNano(), "operation": r.Operation})
			}
			return out, err
		}})
		servers[i] = srv
		holder, _ := store.Endpoint(srv.URL)
		s.Self = holder
		// Bit-reversal spreads the initial 4 nodes, then each doubling.
		s.VNode = tree.VNodeDescriptor{Name: name, ID: id}
	}
	defer func() {
		for _, s := range nodes {
			s.Close()
		}
		for _, srv := range servers {
			srv.Close()
		}
		for _, s := range nodes {
			_ = s.Store.Close()
		}
	}()
	genesis := causal.Genesis{Cluster: "lab", Holder: nodes[0].Self}
	for _, s := range nodes {
		if err := s.Store.InitCausal(nodes[0].VNode, genesis); err != nil {
			t.Fatal(err)
		}
	}
	attachFacade := func(i int) {
		parsed, err := url.Parse(nodes[i].Self.Host)
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(parsed.Port())
		if err != nil {
			t.Fatal(err)
		}
		settings, err := NewSettings(nodes[i].VNode.Name, port, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		settings.SetAdvertise("127.0.0.1")
		facade, err := NewNode(settings, testNewContact, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := facade.EnableCausalCluster(nodes[i]); err != nil {
			t.Fatal(err)
		}
		facades[i] = facade
	}
	if fullNodes {
		for i := range nodes {
			attachFacade(i)
		}
	}
	retryJoin := func(s *mesh.Service) error {
		for {
			err := s.Join(ctx)
			if err == nil {
				return nil
			}
			log.event(map[string]any{"event": "join-attempt-error", "node": s.Self.VNode, "error": err.Error()})
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	for i := 1; i < 4; i++ {
		if err := retryJoin(nodes[i]); err != nil {
			t.Fatal(err)
		}
	}
	prejoined := os.Getenv("INDEXUS_LAB_PREJOIN") == "1"
	if prejoined {
		for i := 4; i < count; i++ {
			if err := retryJoin(nodes[i]); err != nil {
				t.Fatal(err)
			}
		}
		hosts := make([]string, count)
		for i, s := range nodes {
			hosts[i] = s.Self.Host
		}
		if err := nodes[0].Sync(ctx, hosts); err != nil {
			t.Fatal(err)
		}
		for _, s := range nodes[1:] {
			if err := s.Sync(ctx, []string{nodes[0].Self.Host}); err != nil {
				t.Fatal(err)
			}
		}
	}

	if prejoined {
		if err := labReconcilePlacement(ctx, nodes); err != nil {
			t.Fatal("prejoined placement", err)
		}
	}

	initialHosts := make([]string, 4)
	for i := range initialHosts {
		initialHosts[i] = nodes[i].Self.Host
	}
	// Initial joins may have distinct donors; obtain their certified closure
	// before selecting disks for an offline seed. A routing hint is not a permit.
	if err := nodes[0].Sync(ctx, initialHosts); err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes[1:4] {
		if err := node.Sync(ctx, initialHosts); err != nil {
			t.Fatal(err)
		}
	}
	// Offline preload uses the real store's atomic seeding path, before any
	// client writes. Its records are audited separately from client ACKs.
	seedByHost := map[string][]storage.Record{}
	// Offline seeding must use actual installed rights, not desired XOR cells.
	// Earlier joiners may still retain excess scopes after later activations.
	type installedScope struct {
		region tree.Region
		host   string
	}
	var installed []installedScope
	for _, node := range nodes {
		rights, err := node.Store.Rights(tree.Region{})
		if err != nil {
			t.Fatal(err)
		}
		for _, right := range rights {
			installed = append(installed, installedScope{right.Region, node.Self.Host})
		}
	}
	for i := range seedZones {
		record := storage.Record{Collection: fmt.Sprintf("Seed%012d", i%256), Location: uniqueLocation(-i - 1), ID: fmt.Sprintf("seed-%d", i), Payload: json.RawMessage(`{"seed":true}`)}
		z, e := tree.NewZoneKey(record.Collection, record.Location)
		if e != nil {
			t.Fatal(e)
		}
		host := ""
		for _, scope := range installed {
			if scope.region.Contains(z.ID) {
				if host != "" {
					t.Fatal("offline seed discovered overlapping installed rights")
				}
				host = scope.host
			}
		}
		if host == "" {
			t.Fatal("offline seed discovered an uncovered installed scope")
		}
		seedByHost[host] = append(seedByHost[host], record)
	}
	for _, node := range nodes {
		rows := seedByHost[node.Self.Host]
		for len(rows) > 0 {
			n := min(len(rows), 512)
			if err := node.Store.SeedLocalRecords(rows[:n]); err != nil {
				t.Fatal(err)
			}
			rows = rows[n:]
		}
	}
	if fullNodes && seedZones > 0 {
		for _, node := range facades {
			if node.ClientReady() {
				if _, err := node.mesh.summary.Fold(ctx, true); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	members := make([]map[string]any, count)
	for i, node := range nodes {
		members[i] = map[string]any{"index": i, "host": node.Self.Host, "name": node.Self.VNode}
	}
	log.event(map[string]any{"event": "configuration", "members": members, "nodes": count, "hosts": count / 2, "slots": 2, "sync_delay_ms": syncDelay.Milliseconds(), "network_delay_ms": linkDelay.Milliseconds(), "fsync": true, "phase_seconds": phaseTime.Seconds(), "prejoined": prejoined, "unique_zones": uniqueZones, "direct": direct, "base_rate": baseRate, "fixed_rate": envInt("INDEXUS_LAB_RATE", 0), "faults": faults, "stacks": stacks, "write_trace": traceWrites, "cpus": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0), "seed_zones": seedZones, "store_memory_bytes": storeMemory, "gossip_ms": envInt("INDEXUS_LAB_GOSSIP_MS", 1000), "incremental_accounting": os.Getenv("INDEXUS_BENCH_NO_USAGE_SCAN") != "", "full_node_facade": fullNodes, "production_maintenance": fullNodes && maintenance, "tail_rate": tailRate, "request_timeout_ms": requestTimeout.Milliseconds(), "write_attempts": writeAttempts, "note": "single physical host; no claim of EC2 throughput equivalence"})
	gossipCtx, stopGossip := context.WithCancel(ctx)
	var gossipWorkers sync.WaitGroup
	gossipPeriod := time.Duration(envInt("INDEXUS_LAB_GOSSIP_MS", 1000)) * time.Millisecond
	for i, node := range nodes {
		gossipWorkers.Go(func() {
			ticker := time.NewTicker(gossipPeriod)
			defer ticker.Stop()
			var round uint64
			for {
				select {
				case <-gossipCtx.Done():
					return
				case <-ticker.C:
				}
				routing, err := node.Store.CausalRouting()
				if err != nil || !routing.Current().HasName(node.VNode.Name) {
					continue
				}
				if fullNodes {
					if !maintenance {
						continue
					}
					// Use production's separate phase budgets. A short shared
					// deadline would invent recovery starvation in this fixture.
					if err := facades[i].RefreshContext(gossipCtx); err != nil {
						log.event(map[string]any{"event": "maintenance-retry", "node": i, "error": err.Error()})
					}
					updateCtx, done := context.WithTimeout(gossipCtx, 2*time.Second)
					_ = facades[i].UpdateContext(updateCtx)
					done()
				} else {
					round++
					peers, err := node.GossipTargets(round)
					if err != nil {
						continue
					}
					callCtx, stop := context.WithTimeout(gossipCtx, 2*time.Second)
					_ = node.Sync(callCtx, peers)
					stop()
				}
			}
		})
	}
	var stopGossipOnce sync.Once
	finishGossip := func() { stopGossipOnce.Do(func() { stopGossip(); gossipWorkers.Wait() }) }
	defer finishGossip()

	// A client refreshes topology over the same HTTP wire outside its writes.
	// Its view is a routing hint; stale views may relay, never authorize a disk.
	var clientView atomic.Pointer[causal.Membership]
	routingDone := make(chan struct{})
	stopRouting := make(chan struct{})
	go func() {
		defer close(routingDone)
		if !direct {
			return
		}
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			callCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			response, err := transport.Exchange(callCtx, nodes[0].Self.Host, mesh.Request{Kind: "membership", Membership: &mesh.MembershipPacket{}, Owner: nodes[0].Self})
			stop()
			if err == nil && response.Membership != nil && response.Membership.Packet.Body != nil {
				k := response.Membership.Packet.Body
				replica, e := causal.NewMembership(k.Root, k.Genesis)
				if e == nil {
					e = replica.Merge(k.Delta)
				}
				if e == nil {
					clientView.Store(replica)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-stopRouting:
				return
			case <-tick.C:
			}
		}
	}()
	defer func() { close(stopRouting); <-routingDone }()
	if seconds := envInt("INDEXUS_LAB_WARMUP_SECONDS", 0); seconds > 0 {
		log.event(map[string]any{"event": "warmup", "seconds": seconds})
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.Fatal("warmup", ctx.Err())
		}
	}
	start := time.Now()
	log.event(map[string]any{"event": "load-began", "unix_ns": start.UnixNano()})
	var offered, acked, shed, failed, inflight, joined, tailOffered atomic.Int64
	applied := make([]atomic.Int64, count)
	firstDirect := make([]atomic.Bool, count)
	var observedReady, observedOpen, observedFrozen atomic.Int64
	hostIndex := map[string]int{}
	for i, node := range nodes {
		hostIndex[node.Self.Host] = i
	}
	joined.Store(4)
	if prejoined {
		joined.Store(int64(count))
	}
	type job struct {
		id   int64
		zone int
		due  time.Time
	}
	jobs := make(chan job, 4096)
	var writes sync.WaitGroup
	var ackMu sync.Mutex
	ackIDs := map[string]bool{}
	var latencies, intended []int64
	var observations []labWriteObservation
	var hintsMu sync.RWMutex
	hints := map[int]string{}
	// A client that keeps the cell returned with each ACK (WriteRoute.Cell)
	// and sends later writes of that cell straight to its holder.
	cellHints := os.Getenv("INDEXUS_LAB_CELL_HINTS") == "1"
	type cellHint struct {
		region tree.Region
		host   string
	}
	var cells []cellHint
	cellFor := func(id tree.ID) (int, string) {
		best, bits := -1, -1
		for i, c := range cells {
			if c.host == "" || !c.region.Contains(id) {
				continue
			}
			n := 0
			for _, b := range c.region.Mask {
				for ; b != 0; b &= b - 1 {
					n++
				}
			}
			if n > bits {
				best, bits = i, n
			}
		}
		if best < 0 {
			return -1, ""
		}
		return best, cells[best].host
	}
	for range 128 {
		writes.Go(func() {
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				inflight.Add(1)
				started := time.Now()
				observation := labWriteObservation{ID: job.id, DueUS: job.due.Sub(start).Microseconds(), StartUS: started.Sub(start).Microseconds(), Joined: joined.Load(), Ready: observedReady.Load(), Open: observedOpen.Load(), Frozen: observedFrozen.Load()}
				record := storage.Record{Collection: "Demo000000000000", Location: location(job.zone), ID: fmt.Sprintf("w-%d", job.id), Metrics: []float64{1}, Payload: json.RawMessage(`{"lab":"0123456789abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnop"}`)}
				if uniqueZones {
					record.Collection = fmt.Sprintf("Lab%013d", job.zone%256)
					record.Location = uniqueLocation(job.zone)
				}
				ok := false
				hintsMu.RLock()
				target := hints[job.zone]
				hintsMu.RUnlock()
				hinted := -1
				if target == "" && cellHints {
					if z, e := tree.NewZoneKey(record.Collection, record.Location); e == nil {
						hintsMu.RLock()
						hinted, target = cellFor(z.ID)
						hintsMu.RUnlock()
					}
				}
				if target == "" && direct {
					if view := clientView.Load(); view != nil {
						z, e := tree.NewZoneKey(record.Collection, record.Location)
						if e == nil {
							if holder, e := view.Route(z.ID); e == nil {
								target = holder.Host
							}
						}
					}
				}
				if target == "" {
					target = nodes[0].Self.Host
				}
				for attempt := 0; attempt < writeAttempts && ctx.Err() == nil; attempt++ {
					attemptStarted, attemptTarget := time.Now(), target
					reqCtx, stop := context.WithTimeout(ctx, requestTimeout)
					result, e := transport.base.Exchange(reqCtx, target, mesh.Request{Kind: "write", RequestID: record.ID, Record: &record})
					stop()
					observation.Attempts++
					observation.RPCUS += time.Since(attemptStarted).Microseconds()
					if e != nil && traceWrites {
						observation.Errors = append(observation.Errors, e.Error())
					}
					if e != nil {
						target = nodes[0].Self.Host
					}
					if e != nil && failed.Load() < 5 {
						log.event(map[string]any{"event": "write-error", "error": e.Error(), "zone": record.Location})
					}
					if e == nil && result.Applied {
						observation.Direct = attemptTarget == result.AppliedBy.Host
						observation.Degraded = result.Degraded
						if i, found := hostIndex[result.AppliedBy.Host]; found {
							if applied[i].Add(1) == 1 {
								log.event(map[string]any{"event": "first-ack", "node": i, "t": time.Since(start).Seconds(), "direct": target == result.AppliedBy.Host})
							}
							if target == result.AppliedBy.Host && firstDirect[i].CompareAndSwap(false, true) {
								log.event(map[string]any{"event": "first-direct-ack", "node": i, "t": time.Since(start).Seconds()})
							}
						}
						hintsMu.Lock()
						hints[job.zone] = result.AppliedBy.Host
						if cellHints {
							if hinted >= 0 && hinted < len(cells) && cells[hinted].host != result.AppliedBy.Host {
								cells[hinted].host = "" // it no longer serves this address
							}
							if result.AppliedRegion != nil {
								found := false
								for i := range cells {
									if cells[i].region == *result.AppliedRegion {
										cells[i].host, found = result.AppliedBy.Host, true
									}
								}
								if !found && len(cells) < 4096 {
									cells = append(cells, cellHint{*result.AppliedRegion, result.AppliedBy.Host})
								}
							}
						}
						hintsMu.Unlock()
						ok = true
						break
					}
					backoffStarted := time.Now()
					timer := time.NewTimer(time.Duration(100*(attempt+1)) * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
					case <-timer.C:
					}
					observation.BackoffUS += time.Since(backoffStarted).Microseconds()
				}
				observation.FinishUS = time.Since(start).Microseconds()
				observation.LatencyUS = time.Since(started).Microseconds()
				observation.IntendedUS = time.Since(job.due).Microseconds()
				observation.Success = ok
				ackMu.Lock()
				if traceWrites {
					observations = append(observations, observation)
				}
				if ok {
					acked.Add(1)
					ackIDs[record.ID] = true
					latencies = append(latencies, observation.LatencyUS)
					intended = append(intended, observation.IntendedUS)
				} else {
					failed.Add(1)
				}
				ackMu.Unlock()
				inflight.Add(-1)
			}
		})
	}
	stopSample := make(chan struct{})
	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stopSample:
				return
			case tickAt := <-tick.C:
				sampleStarted := time.Now()
				if age := int(time.Since(start).Seconds()); stacks && age > 0 && age%30 == 0 {
					if f, err := os.Create(filepath.Join(out, fmt.Sprintf("stacks-%04d.txt", age))); err == nil {
						_ = pprof.Lookup("goroutine").WriteTo(f, 2)
						_ = f.Close()
					}
				}
				states := make([]map[string]any, 0, count)
				var ready, open, frozenCount int64
				for i, s := range nodes {
					routing, e := s.Store.CausalRouting()
					if e != nil {
						continue
					}
					frozen, _ := s.Store.FrozenOutgoing()
					frozenCount += int64(len(frozen))
					if routing.HasOpenTransfer() {
						open++
					}
					stats := s.Store.CommitStats()
					state := map[string]any{"node": i, "applied": applied[i].Load(), "open": routing.HasOpenTransfer(), "frozen": frozen, "writer": stats.Stages, "commits_by_kind": stats.ByKind, "pending": stats.Pending}
					if fullNodes {
						state["client_ready"] = facades[i].ClientReady()
						if facades[i].ClientReady() {
							ready++
						}
						state["summary"] = facades[i].SummaryMetrics()
					}
					states = append(states, state)
				}
				observedReady.Store(ready)
				observedOpen.Store(open)
				observedFrozen.Store(frozenCount)
				var memory runtime.MemStats
				runtime.ReadMemStats(&memory)
				log.event(map[string]any{"event": "sample", "t": time.Since(start).Seconds(), "offered": offered.Load(), "acked": acked.Load(), "shed": shed.Load(), "failed": failed.Load(), "in_flight": inflight.Load(), "queued": len(jobs), "joined": joined.Load(), "ready": ready, "open": open, "frozen_count": frozenCount, "heap_bytes": memory.HeapAlloc, "gc_cycles": memory.NumGC, "gc_pause_ns": memory.PauseTotalNs, "goroutines": runtime.NumGoroutine(), "tick_lag_us": sampleStarted.Sub(tickAt).Microseconds(), "sample_us": time.Since(sampleStarted).Microseconds(), "rpc_calls": transport.calls.Load(), "rpc_failures": transport.failed.Load(), "tail_offered": tailOffered.Load(), "nodes": states})
			}
		}
	}()
	var joins sync.WaitGroup
	joinErrors := make(chan error, count)
	joinSlots := make(chan struct{}, 8)
	next := 4
	if prejoined {
		next = count
	}
	var sequence int64
	for p, rate := range []int{baseRate, 2 * baseRate, 4 * baseRate, 8 * baseRate} {
		if fixed := envInt("INDEXUS_LAB_RATE", 0); fixed > 0 {
			rate = fixed
		}
		if p == 1 && faults && count > 6 {
			blockedUntil[6].Store(time.Now().Add(750 * time.Millisecond).UnixNano())
			log.event(map[string]any{"event": "partition", "node": 6, "duration_ms": 750})
		}
		if p == 3 && faults {
			devices[0].slowDelay.Store(int64(25 * time.Millisecond))
			devices[0].slowUntil.Store(time.Now().Add(750 * time.Millisecond).UnixNano())
			log.event(map[string]any{"event": "io-tail", "host": 0, "extra_sync_ms": 25, "duration_ms": 750})
		}
		target := min(count, 8<<p)
		for next < target {
			node := nodes[next]
			next++
			joins.Go(func() {
				select {
				case joinSlots <- struct{}{}:
				case <-ctx.Done():
					joinErrors <- ctx.Err()
					return
				}
				defer func() { <-joinSlots }()
				if err := retryJoin(node); err != nil {
					joinErrors <- err
				} else {
					joined.Add(1)
					log.event(map[string]any{"event": "join-complete", "node": node.Self.VNode, "t": time.Since(start).Seconds()})
				}
			})
		}
		log.event(map[string]any{"event": "step", "t": time.Since(start).Seconds(), "rate": rate, "join_target": target})
		tick := time.NewTicker(10 * time.Millisecond)
		began := time.Now()
		scheduled := 0
		total := int(float64(rate) * phaseTime.Seconds())
		for scheduled < total && ctx.Err() == nil {
			<-tick.C
			elapsed := min(time.Since(began), phaseTime)
			expected := min(total, int(float64(rate)*elapsed.Seconds()))
			for scheduled < expected {
				scheduled++
				sequence++
				offered.Add(1)
				zone := int(sequence % 4096)
				if uniqueZones {
					zone = int(sequence)
				}
				select {
				case jobs <- job{sequence, zone, began.Add(time.Duration(float64(scheduled) / float64(rate) * float64(time.Second)))}:
				default:
					shed.Add(1)
				}
			}
		}
		tick.Stop()
	}
	// Keep a low stream of fresh writes alive while the final joins acquire
	// their remaining pieces. This distinguishes readiness from an idle node
	// that only becomes available after the offered phase has stopped.
	if tailRate > 0 {
		done := make(chan struct{})
		go func() { joins.Wait(); close(done) }()
		began := time.Now()
		tick := time.NewTicker(10 * time.Millisecond)
		var scheduled int64
	tail:
		for {
			select {
			case <-ctx.Done():
				break tail
			case <-done:
				break tail
			case <-tick.C:
				expected := int64(time.Since(began).Seconds() * float64(tailRate))
				for scheduled < expected {
					scheduled++
					sequence++
					offered.Add(1)
					tailOffered.Add(1)
					zone := int(sequence % 4096)
					if uniqueZones {
						zone = int(sequence)
					}
					select {
					case jobs <- job{sequence, zone, began.Add(time.Duration(float64(scheduled) / float64(tailRate) * float64(time.Second)))}:
					default:
						shed.Add(1)
					}
				}
			}
		}
		tick.Stop()
	}
	close(jobs)
	writes.Wait()
	if traceWrites {
		if err := labSaveObservations(filepath.Join(out, "writes.jsonl"), observations); err != nil {
			t.Fatal(err)
		}
	}
	joins.Wait()
	finishGossip()
	close(joinErrors)
	for err := range joinErrors {
		if err != nil {
			close(stopSample)
			<-sampleDone
			t.Fatal(err)
		}
	}
	close(stopSample)
	<-sampleDone
	if acked.Load() < 128 {
		t.Fatalf("vacuous audit: only %d ACKs", acked.Load())
	}
	if joined.Load() != int64(count) {
		t.Fatalf("only %d joins completed", joined.Load())
	}
	hosts := make([]string, count)
	for i, s := range nodes {
		hosts[i] = s.Self.Host
	}
	if err := nodes[0].Sync(ctx, hosts); err != nil {
		t.Fatal(err)
	}
	for _, s := range nodes[1:] {
		if err := s.Sync(ctx, []string{nodes[0].Self.Host}); err != nil {
			t.Fatal(err)
		}
	}
	// Join serves installed pieces immediately and is relative to its local
	// membership view. Once the load and joins stop, explicitly reconcile the
	// final placement for this laboratory's placement audit. This is outside
	// the protocol's cutover path and does not gate first traffic.
	log.event(map[string]any{"event": "placement-reconcile-began", "t": time.Since(start).Seconds()})
	if err := labReconcilePlacement(ctx, nodes); err != nil {
		t.Fatal("final desired placement", err)
	}
	log.event(map[string]any{"event": "placement-reconcile-completed", "t": time.Since(start).Seconds()})
	// Check the actual local permission frontier after maintenance; routing
	// membership carries no certificate or consumed-right history anymore.
	for i, s := range nodes {
		if err := s.Resume(ctx); err != nil {
			t.Fatalf("local permission maintenance on vnode %d: %v", i, err)
		}
		state, err := s.Store.CausalState()
		if err != nil {
			t.Fatal(err)
		}
		if state.Permissions.Validate(s.Self.Owner) != nil {
			t.Fatalf("vnode %d lacks a valid installed local right", i)
		}
		log.event(map[string]any{"event": "local-permissions", "node": i, "native_rights": len(state.Permissions.Native), "sequence": state.Permissions.Sequence, "operations": len(state.Transfers)})
	}
	if err := nodes[0].Sync(ctx, hosts); err != nil {
		t.Fatal(err)
	}
	for _, s := range nodes[1:] {
		if err := s.Sync(ctx, []string{nodes[0].Self.Host}); err != nil {
			t.Fatal(err)
		}
	}
	// Reopen one completed receiver on exactly the same durable directory.
	// Mid-flight persistence is covered separately by store cut tests/TLA.
	victim := count - 1
	gates[victim].Lock()
	old := nodes[victim]
	old.Close()
	if err := old.Store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(out, fmt.Sprintf("lab-%02d", victim), "store")
	reopened, err := storage.OpenWithOptions(path, old.Self.VNode, "lab", 1<<30, storage.Options{FS: disk{zonelog.Dir(storage.RecordsDir(path)), devices[victim/2]}, BatchWrites: true, BatchSize: 128, BatchDelay: time.Millisecond, MemoryBytes: storeMemory})
	if err != nil {
		t.Fatal(err)
	}
	nodes[victim] = &mesh.Service{Store: reopened, Self: old.Self, VNode: old.VNode, Peers: transport}
	if fullNodes {
		attachFacade(victim)
	}
	gates[victim].Unlock()
	log.event(map[string]any{"event": "reopened", "node": victim, "same_storage_identity": true})
	rows, err := nodes[0].Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, snap := range rows {
		for _, r := range snap.Records {
			seen[r.ID]++
		}
	}
	for i := range seedZones {
		if seen[fmt.Sprintf("seed-%d", i)] != 1 {
			t.Fatalf("seed %d lost or duplicated", i)
		}
	}

	for id := range ackIDs {
		if seen[id] != 1 {
			t.Fatalf("ACK %s observed %d times", id, seen[id])
		}
	}
	for _, s := range nodes {
		routing, err := s.Store.CausalRouting()
		if err != nil || routing.HasOpenTransfer() {
			t.Fatalf("unfinished durable transfer on %s: %v", s.Self.VNode, err)
		}
		f, e := s.Store.FrozenOutgoing()
		if e != nil || len(f) != 0 {
			t.Fatalf("unclosed fences on %s: %v %v", s.Self.VNode, f, e)
		}
	}
	// Query intentionally coalesces copies. Independently enumerate every
	// disk's writable inventory, so deduplication cannot hide two live owners.
	owned := map[string]int{}
	for _, node := range nodes {
		var local map[string]int
		for {
			local = map[string]int{}
			request := storage.InventoryPageRequest{MetricsOnly: true}
			retry := false
			for {
				rows, page, err := node.Store.CausalInventoryPage(ctx, nil, request)
				if errors.Is(err, storage.ErrReadChanged) {
					retry = true
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, snap := range rows {
					for _, record := range snap.Records {
						local[record.ID]++
					}
				}
				if page.Done {
					break
				}
				request.After = page.Next
			}
			if !retry {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
		for id, count := range local {
			owned[id] += count
		}
	}
	for id := range ackIDs {
		if owned[id] != 1 {
			t.Fatalf("ACK %s has %d writable copies", id, owned[id])
		}
	}
	for i := range seedZones {
		if owned[fmt.Sprintf("seed-%d", i)] != 1 {
			t.Fatalf("seed %d has %d writable copies", i, owned[fmt.Sprintf("seed-%d", i)])
		}
	}

	for zone := 0; zone < 16; zone++ {
		record := storage.Record{Collection: "Demo000000000000", Location: location(zone * 251), ID: fmt.Sprintf("authority-probe-%d", zone)}
		writers := 0
		for _, node := range nodes {
			if _, err := node.Store.Write(record.ID, record); err == nil {
				writers++
			}
		}
		if writers != 1 {
			t.Fatalf("zone %s has %d ACK writers", record.Location, writers)
		}
	}

	latencySummary := func(values []int64) map[string]any {
		slices.Sort(values)
		if len(values) == 0 {
			return map[string]any{"count": 0}
		}
		var sum int64
		for _, v := range values {
			sum += v
		}
		return map[string]any{"count": len(values), "mean_us": sum / int64(len(values)), "p50_us": values[len(values)/2], "p95_us": values[(len(values)-1)*95/100], "p99_us": values[(len(values)-1)*99/100], "max_us": values[len(values)-1]}
	}
	log.event(map[string]any{"event": "client-latency", "rpc_attempts_per_write": writeAttempts, "request_timeout_ms": requestTimeout.Milliseconds(), "ack_latency": latencySummary(latencies), "intended_latency": latencySummary(intended)})

	summary := map[string]any{"event": "audit", "nodes": count, "fsync": true, "offered": offered.Load(), "acked": acked.Load(), "shed": shed.Load(), "failed": failed.Load(), "observed_records": len(seen), "elapsed_s": time.Since(start).Seconds(), "ack_missing": 0, "ack_duplicates": 0, "writable_ack_duplicates": 0, "writable_ack_missing": 0, "frozen_remaining": 0, "prejoined": prejoined, "unique_zones": uniqueZones, "direct": direct, "seed_zones": seedZones, "base_rate": baseRate, "tail_offered": tailOffered.Load()}
	log.event(map[string]any{"event": "wire", "counters": metrics.Snapshot()["mesh_sent"]})
	log.event(map[string]any{"event": "membership-transport", "counters": metrics.Snapshot()["gossip"]})
	log.event(map[string]any{"event": "handoff-timings", "counters": metrics.Snapshot()["handoffs"]})
	for i, d := range devices {
		log.event(map[string]any{"event": "device", "host": i, "syncs": d.syncs.Load(), "wait_ms": d.waitNS.Load() / int64(time.Millisecond), "held_ms": d.holdNS.Load() / int64(time.Millisecond)})
	}
	log.event(summary)
	t.Logf("%v", summary)
	if failed.Load() != 0 || shed.Load() != 0 || acked.Load() != offered.Load() {
		t.Fatalf("availability audit: offered=%d acked=%d failed=%d shed=%d", offered.Load(), acked.Load(), failed.Load(), shed.Load())
	}
}
func location(i int) string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	return string([]byte{alphabet[i%64], alphabet[(i/64)%64], alphabet[(i/4096)%64]})
}

// Match the AWS insert workload: 96 independent address bits and 256 collections.
// Hashing the sequence keeps the fixture repeatable without concentrating keys
// in the few leading bits of an integer encoding.
func uniqueLocation(i int) string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	sum := sha256.Sum256([]byte(strconv.Itoa(i)))
	out := make([]byte, 16)
	for j := range out {
		out[j] = alphabet[sum[j]&63]
	}
	return string(out)
}
