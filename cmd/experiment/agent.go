package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/autoscale"
)

// agent manages node slots on one machine, so one machine can
// host several vnodes and a cluster is rebuilt without recreating machines:
//
//	POST /spawn[?genesis=1&wait=ready&prefer_near=&compaction=&items=&provisioner=&bootstrap=]
//	POST /restart?slot=i   crash (SIGKILL) and restart a slot on the same disk
//	POST /kill?slot=i      crash a slot and leave it down
//	POST /reset            kill every slot and wipe the disks
//	GET  /count            "running free"
//	GET  /facts            resource facts of the machine, for placement
//	GET  /slots            per-slot state
//
// The provisioner's spawn hook (ops/experiment/elastic) calls /spawn, so a
// scale-up is bounded by the slots paid for. A slot is free again once its
// process exits: a retired vnode leaving (retired_exit_seconds) or a crash.
type slot struct {
	ports    nodePorts
	options  spawnOptions
	cmd      *exec.Cmd
	started  time.Time
	exited   time.Time
	exitCode int
	done     chan struct{}
	// What the node's own /metrics said lately (see observeSlot).
	name    string
	joining bool
	cpu     []cpuSample
	// items is the node's advisory owned item count, when it reports one.
	items        int64
	itemsOK      bool
	itemsAt      time.Time
	joinCores    float64
	joinSampleAt time.Time
}

// cpuSample is a node's cumulative CPU time read at a given moment.
type cpuSample struct {
	at      time.Time
	seconds float64
	applied int64 // -1 if the node omitted its cumulative applied-write count
}

// factsEvery and cpuSamples make the CPU window of /facts: the last
// cpuSamples readings, one every factsEvery (10 s). Short enough to see a
// join start, long enough not to follow one commit burst.
const (
	factsEvery = 2 * time.Second
	cpuSamples = 6
)

// record keeps what the node reported; the caller holds the agent's mutex.
func (s *slot) record(name string, joining bool, cpuSeconds float64, applied *int64, now time.Time) {
	if name != s.name {
		s.cpu = nil
		s.joinSampleAt = time.Time{}
	}
	s.name, s.joining = name, joining
	count := int64(-1)
	if applied != nil {
		count = *applied
	}
	s.cpu = append(s.cpu, cpuSample{at: now, seconds: cpuSeconds, applied: count})
	if len(s.cpu) > cpuSamples {
		s.cpu = s.cpu[1:]
	}
	if joining && len(s.cpu) == cpuSamples {
		first := s.cpu[0]
		if elapsed := now.Sub(first.at); elapsed >= factsEvery*(cpuSamples-1)/2 && elapsed <= 2*factsEvery*(cpuSamples-1) && cpuSeconds >= first.seconds {
			s.joinCores, _ = s.cores()
			s.joinSampleAt = now
		}
	}
}

// cores is the CPU the node used over its window, in cores; ok is false
// until two readings exist.
func (s *slot) cores() (float64, bool) {
	if len(s.cpu) < 2 {
		return 0, false
	}
	first, last := s.cpu[0], s.cpu[len(s.cpu)-1]
	elapsed := last.at.Sub(first.at).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	return max(0, last.seconds-first.seconds) / elapsed, true
}

// appliedRate uses a full, recent observation window of the same process.
// A counter reset or an unreadable counter invalidates that window.
func (s *slot) appliedRate(now time.Time) (autoscale.AppliedRate, bool) {
	if s.name == "" || len(s.cpu) != cpuSamples {
		return autoscale.AppliedRate{}, false
	}
	first, last := s.cpu[0], s.cpu[len(s.cpu)-1]
	window := last.at.Sub(first.at)
	nominal := factsEvery * (cpuSamples - 1)
	if window < nominal/2 || window > nominal*2 || now.Sub(last.at) > autoscale.AppliedRateMaxAge || last.at.After(now) || first.applied < 0 {
		return autoscale.AppliedRate{}, false
	}
	for i := 1; i < len(s.cpu); i++ {
		if s.cpu[i].applied < s.cpu[i-1].applied {
			return autoscale.AppliedRate{}, false
		}
	}
	return autoscale.AppliedRate{PerSecond: float64(last.applied-first.applied) / window.Seconds(), AsOf: last.at}, true
}

// existingDir is dir or its nearest existing parent: the slot directory is
// only created by the first spawn, and its disk is measured before.
func existingDir(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil || dir == filepath.Dir(dir) {
			return dir
		}
		dir = filepath.Dir(dir)
	}
}

// recordItems never publishes an uncounted or stale zero as an empty vnode.
func (s *slot) recordItems(items *int64, counted bool, ageMS int64, now time.Time) {
	s.itemsOK = items != nil && *items >= 0 && counted && ageMS >= 0 && ageMS <= autoscale.ItemsCountMaxAge.Milliseconds()
	if s.itemsOK {
		s.items, s.itemsAt = *items, now.Add(-time.Duration(ageMS)*time.Millisecond)
	}
}

// machineFacts sums the slots into what placement reads. A live node whose
// CPU is not known yet (just started, or an old node that does not answer
// while it joins) counts as receiving: its machine is paying for a join.
func machineFacts(slots []*slot, cores int, host autoscale.ResourceSample, addressSlots int, dynamic bool) autoscale.MachineFacts {
	f := autoscale.MachineFacts{Cores: cores, CPUOK: true, AddressSlots: addressSlots, ResourceManaged: dynamic}
	used := 0.0
	now := time.Now()
	rate := autoscale.AppliedRate{AsOf: now}
	complete := true
	f.ReceiverReserveCores = autoscale.MinReceiverCores
	for _, s := range slots {
		if s == nil || !s.exited.IsZero() {
			f.Free++
			continue
		}
		f.Running++
		if !s.joinSampleAt.IsZero() && now.Sub(s.joinSampleAt) <= time.Minute {
			f.ReceiverReserveCores = max(f.ReceiverReserveCores, s.joinCores)
		}
		if s.name != "" {
			f.VNodes = append(f.VNodes, s.name)
			if s.itemsOK && time.Since(s.itemsAt) <= autoscale.ItemsCountMaxAge {
				if f.Items == nil {
					f.Items = map[string]int64{}
				}
				f.Items[s.name] = s.items
			}
		}
		if slotRate, ok := s.appliedRate(now); ok {
			rate.PerSecond += slotRate.PerSecond
			if slotRate.AsOf.Before(rate.AsOf) {
				rate.AsOf = slotRate.AsOf
			}
		} else {
			complete = false
		}
		c, measured := s.cores()
		used += c
		// In resource mode a process that stopped answering is an unknown
		// receiver until a fresh observation arrives.
		if !measured || (dynamic || addressSlots > 0) && now.Sub(s.cpu[len(s.cpu)-1].at) > cpuSamples*factsEvery {
			f.UnobservedSlots++
			f.Receiving++
		} else if s.joining {
			f.Receiving++
		}
	}
	if dynamic {
		f.Free = 1
	} else if addressSlots > 0 {
		f.Free = addressSlots - f.Running
	}
	if complete && f.Running > 0 {
		f.AppliedRate = &rate
	}
	f.CPUPct = 100 * used / float64(max(cores, 1))
	f.MemOK = host.MemTotal > 0
	f.MemFreePct = 100 - host.MemPct
	f.DiskOK, f.DiskFreePct = host.DiskOK, host.DiskFreePct
	return f
}

// An address may be unoccupied while its machine cannot accept a receiver.
// Resource mode exposes at most one immediately admissible slot through /count.
func offeredSlots(f autoscale.MachineFacts) int {
	if f.AddressSlots == 0 && !f.ResourceManaged {
		return f.Free
	}
	if ready, _ := autoscale.ReceiverOffer(f); ready {
		return 1
	}
	return 0
}

type spawnOptions struct {
	Genesis     bool   `json:"genesis,omitempty"`
	PreferNear  string `json:"prefer_near,omitempty"`
	Compaction  bool   `json:"compaction"`
	Items       int    `json:"items_per_vnode,omitempty"`
	Provisioner string `json:"provisioner,omitempty"`
	Bootstrap   string `json:"bootstrap,omitempty"`
	RetiredExit int    `json:"retired_exit_seconds,omitempty"`
	Replication string `json:"replication,omitempty"`
	// ReplicaMachine: "endpoint" when the agent's slots share one host
	// and should count as machines of their own.
	ReplicaMachine string `json:"replication_machine,omitempty"`
}

type agentConfig struct {
	slots                int
	resourceSlots        bool
	node, dir, advertise string
	issuer               string
	p2pBase, monBase     int
	debugBase            int
	dataBytes            int64
	defaults             spawnOptions
	host                 string
	ca                   string // with a CA directory, slots serve TLS
	writeCap             float64
	writeDemand          float64
	cpuTarget            float64
	resourceSignals      bool
	copySlots            int
}

func agent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	listen := fs.String("listen", ":22100", "agent HTTP address")
	var c agentConfig
	fs.BoolVar(&c.resourceSlots, "resource-slots", false, "admit new processes by machine resources; -slots=0 grows with demand")
	fs.IntVar(&c.slots, "slots", 4, "fixed address positions, or zero for dynamic endpoints in resource mode")
	idleExit := fs.Duration("idle-exit", 0, "stop the agent (release the machine) once it has run nodes and none has been alive for this long; 0 never")
	fs.StringVar(&c.node, "node", "node", "node binary")
	fs.StringVar(&c.dir, "dir", "/tmp/indexus-slots", "slot data directory")
	fs.StringVar(&c.advertise, "advertise", "127.0.0.1", "address peers reach this machine at")
	fs.StringVar(&c.issuer, "issuer", "", "issuer URL")
	fs.StringVar(&c.defaults.Bootstrap, "bootstrap", "", "default bootstrap p2p URL for joining nodes")
	fs.StringVar(&c.defaults.Provisioner, "provisioner", "", "default provisioner URL; empty disables scaling")
	fs.IntVar(&c.p2pBase, "p2p-base", 21100, "slot i listens on p2p-base+i")
	fs.IntVar(&c.monBase, "mon-base", 19100, "slot i monitors on mon-base+i")
	fs.IntVar(&c.debugBase, "debug-base", 0, "slot i serves loopback profiling on debug-base+i (node built with -tags diagnostic); 0 is off")
	fs.Int64Var(&c.dataBytes, "data-bytes", 1<<30, "limits.data_bytes per node")
	fs.BoolVar(&c.defaults.Compaction, "compaction", true, "default: let cool vnodes fold (COLD-2)")
	// Off by default: on 2026-11-15 a retired vnode that exited 60 s after its
	// witnessed retirement was still contacted by rebalance while some zones
	// stayed fenced. Until that is understood, retired vnodes keep forwarding.
	fs.IntVar(&c.defaults.RetiredExit, "retired-exit", 0, "seconds a retired vnode keeps forwarding before it exits and frees its slot; 0 keeps it (default)")
	fs.StringVar(&c.defaults.Replication, "replication", "", "default node replication: \"\" off, sync or ack")
	fs.StringVar(&c.defaults.ReplicaMachine, "replication-machine", "", "default replication_machine: \"\" (host) or endpoint")
	fs.IntVar(&c.defaults.Items, "items-per-vnode", 0, "default owned-item scale threshold; resource signals controlled by -resource-signals")
	fs.Float64Var(&c.writeCap, "write-cap", 0, "limits.writes_per_second per node (CAP-2): makes the vnode, not the host, the bottleneck; 0 is no cap")
	fs.Float64Var(&c.writeDemand, "writes-per-vnode", 0, "scale-up threshold in demanded writes/s, independent of the write cap; 0 disables")
	fs.Float64Var(&c.cpuTarget, "cpu-target", 0, "sustained CPU percentage for resource-driven relief (-resource-signals); 0 uses the node default")
	fs.BoolVar(&c.resourceSignals, "resource-signals", false, "keep CPU, memory and disk scale signals active alongside item/write thresholds")
	fs.IntVar(&c.copySlots, "copy-slots", 0, "transfer copies at once on this machine, shared by its nodes (HOST-COPY-1); 0 is a quarter of cores, negative no budget")
	fs.StringVar(&c.ca, "ca", "", "CA directory from `experiment ca`; slots then serve TLS and HTTP/3 (NET-1)")
	_ = fs.Parse(args)
	if c.resourceSlots {
		explicitSlots := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "slots" {
				explicitSlots = true
			}
		})
		if !explicitSlots {
			c.slots = 0
		}
	}
	if c.slots < 0 || (c.slots == 0 && !c.resourceSlots) {
		return fmt.Errorf("zero slots requires resource admission; fixed slots must be positive")
	}
	if !(c.cpuTarget >= 0 && c.cpuTarget <= 100) {
		return fmt.Errorf("agent -cpu-target must be between 0 and 100")
	}
	scheme := "http"
	if c.ca != "" {
		scheme = "https"
		// The agent, its nodes and their peers all trust the run's CA.
		_ = os.Setenv("SSL_CERT_FILE", filepath.Join(c.ca, "ca.crt"))
	}
	if c.issuer == "" {
		return fmt.Errorf("agent needs -issuer")
	}
	// Publish exactly the positive limit written into every node config.
	// Resolve the shared directory once so reporting and enforcement agree.
	copyDir := ""
	if c.copySlots >= 0 {
		var err error
		copyDir, err = filepath.Abs(filepath.Join(c.dir, "copy-budget"))
		if err != nil {
			return err
		}
		if c.copySlots == 0 {
			c.copySlots = indexus.DefaultCopySlots()
		}
	}
	c.host, _ = os.Hostname()
	hostCPU := &hostCPUMeter{}
	hostCPU.sample()
	slots := make([]*slot, c.slots)
	// An unused address costs one pointer and has no process, disk or monitor.
	listed := c.slots
	addressSlots := 0
	if c.resourceSlots {
		addressSlots, listed = c.slots, 0
	}
	var mu sync.Mutex
	resetting := false
	alive := func(s *slot) bool { return s != nil && s.exited.IsZero() }

	writeConfig := func(dir string, ports nodePorts, o spawnOptions) (string, error) {
		network := map[string]any{"listen": fmt.Sprintf("0.0.0.0:%d", ports.p2p), "monitoring": fmt.Sprintf("0.0.0.0:%d", ports.monitor), "advertise": []string{c.advertise}}
		if !o.Genesis {
			network["bootstrap"] = []string{o.Bootstrap}
		}
		identity := map[string]any{"network": "experiment", "cluster": "experiment", "issuer": c.issuer}
		if o.PreferNear != "" {
			identity["prefer_near"] = o.PreferNear // a target: each attempt gets a fresh name (SPAWN-1)
		}
		limits := map[string]any{"data_bytes": c.dataBytes, "writes_per_second": c.writeCap}
		if copyDir != "" {
			// One directory for every slot: the nodes of this machine share it.
			limits["copy_budget_directory"], limits["copy_slots"] = copyDir, c.copySlots
		}
		cfg := map[string]any{
			"storage":  map[string]any{"path": dir + "/backup", "identity_directory": dir},
			"identity": identity, "network": network,
			"limits":     limits,
			"compaction": o.Compaction,
		}
		if o.RetiredExit > 0 {
			cfg["retired_exit_seconds"] = o.RetiredExit
		}
		if o.Replication != "" {
			cfg["replication"] = o.Replication
			if o.ReplicaMachine != "" {
				cfg["replication_machine"] = o.ReplicaMachine
			}
		}
		if o.Provisioner != "" {
			cfg["provisioner"] = o.Provisioner
			if o.Items > 0 || c.writeDemand > 0 || c.resourceSignals || c.cpuTarget > 0 {
				cfg["scaling"] = map[string]any{"items_per_vnode": o.Items, "writes_per_vnode": c.writeDemand, "resource_signals": c.resourceSignals, "cpu_target_pct": c.cpuTarget}
			}
		}
		if c.ca != "" {
			if err := issue(c.ca, dir, []string{c.advertise, "127.0.0.1"}); err != nil {
				return "", err
			}
			network["tls_directory"] = dir
		}
		raw, _ := json.MarshalIndent(cfg, "", "  ")
		path := dir + "/node.json"
		return path, os.WriteFile(path, raw, 0o644)
	}

	// keepLog moves slot i's node log out of the slot before its disk is
	// wiped: the passes of a campaign reuse the slots, and the logs of
	// every pass are read after it (history/<unix time>-<slot>/node.log).
	keepLog := func(i int) {
		from := filepath.Join(c.dir, strconv.Itoa(i), "node.log")
		if _, err := os.Stat(from); err != nil {
			return
		}
		to := filepath.Join(filepath.Dir(c.dir), "history", fmt.Sprintf("%d-%d", time.Now().UnixNano(), i))
		if os.MkdirAll(to, 0o755) == nil {
			_ = os.Rename(from, filepath.Join(to, "node.log"))
		}
	}

	// start runs slot i's node; the caller holds mu. fresh wipes the disk.
	start := func(i int, o spawnOptions, fresh bool) error {
		var ports nodePorts
		if !fresh && slots[i] != nil {
			ports = slots[i].ports
		} else {
			var err error
			ports, err = c.portsFor(i, slots)
			if err != nil {
				return err
			}
		}
		dir := filepath.Join(c.dir, strconv.Itoa(i))
		if fresh {
			keepLog(i)
			_ = os.RemoveAll(dir)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path, err := writeConfig(dir, ports, o)
		if err != nil {
			return err
		}
		logf, err := os.OpenFile(dir+"/node.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if fresh && o.Genesis {
			init := exec.Command(c.node, "init", "-config", path)
			init.Stdout, init.Stderr = logf, logf
			if err := init.Run(); err != nil {
				logf.Close()
				return fmt.Errorf("genesis init: %w", err)
			}
		}
		cmd := exec.Command(c.node, "serve", "-config", path)
		cmd.Env = append(os.Environ(), "INDEXUS_DEBUG_STACKS=1") // /debug/stacks for the sentinel
		if os.Getenv("GOMEMLIMIT") == "" {
			// The slots of this machine share its memory.
			peers := c.slots
			if c.resourceSlots {
				// Share initially by hardware parallelism, then by the live density.
				// The port range carries no memory reservation.
				live := 0
				for _, s := range slots[:listed] {
					if alive(s) {
						live++
					}
				}
				peers = max(runtime.NumCPU(), live+1)
			}
			cmd.Env = append(cmd.Env, fmt.Sprintf("GOMEMLIMIT=%d", autoscale.HeapShareOfHost(peers)))
		}
		if c.debugBase > 0 {
			cmd.Env = append(cmd.Env, fmt.Sprintf("INDEXUS_LOCAL_DEBUG_PORT=%d", ports.debug))
		}
		cmd.Stdout, cmd.Stderr = logf, logf
		if err := cmd.Start(); err != nil {
			logf.Close()
			return err
		}
		s := &slot{ports: ports, options: o, cmd: cmd, started: time.Now(), done: make(chan struct{})}
		if i == len(slots) {
			slots = append(slots, s)
		} else {
			slots[i] = s
		}
		listed = max(listed, i+1)
		go func() {
			err := cmd.Wait()
			logf.Close()
			mu.Lock()
			s.exited, s.exitCode = time.Now(), cmd.ProcessState.ExitCode()
			mu.Unlock()
			close(s.done)
			log.Printf("slot %d exited after %s: %v", i, time.Since(s.started).Round(time.Second), err)
		}()
		log.Printf("slot %d started %+v", i, o)
		return nil
	}

	// wait polls slot i's status: up, or client ready so that the next
	// joiner's JOIN-1 hint already sees it.
	wait := func(s *slot, ready bool) error {
		ports := s.ports
		url := fmt.Sprintf("%s://127.0.0.1:%d/status", scheme, ports.monitor)
		for t := 0; t < 720; t++ {
			select {
			case <-s.done:
				return fmt.Errorf("node exited before becoming available (exit %d)", s.exitCode)
			default:
			}
			if res, err := client.Get(url); err == nil {
				body, _ := io.ReadAll(res.Body)
				res.Body.Close()
				if !ready || res.StatusCode == http.StatusOK && strings.Contains(string(body), `"client_ready":true`) {
					return nil
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		return fmt.Errorf("node status timed out")
	}

	crash := func(i int) (*slot, error) {
		mu.Lock()
		s := slots[i]
		if alive(s) {
			_ = s.cmd.Process.Signal(syscall.SIGKILL)
		}
		mu.Unlock()
		if s == nil {
			return nil, fmt.Errorf("slot %d never started", i)
		}
		<-s.done
		return s, nil
	}

	slotArg := func(r *http.Request) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		i, err := strconv.Atoi(r.URL.Query().Get("slot"))
		if err != nil || i < 0 || i >= len(slots) {
			return 0, fmt.Errorf("bad slot")
		}
		return i, nil
	}

	// Read the machine without holding the process table lock. Combine these
	// measurements with the latest slots while checking and starting a process.
	var hostMu sync.Mutex
	var sampled time.Time
	var capacity autoscale.ResourceSample
	var measured hostCPUObservation
	readHost := func() (autoscale.ResourceSample, hostCPUObservation) {
		hostMu.Lock()
		defer hostMu.Unlock()
		// In resource mode /count, /facts, and /spawn use one measurement
		// per normal observation interval. Frequent admission requests must
		// not turn a machine CPU window into a single kernel tick.
		if c.resourceSlots && time.Since(sampled) < factsEvery {
			cpu := measured
			if cpu.valid {
				cpu.Status = "reused"
			}
			return capacity, cpu
		}
		capacity, measured = autoscale.SampleCapacity(existingDir(c.dir)), hostCPU.sample()
		sampled = time.Now()
		return capacity, measured
	}
	factsLocked := func(host autoscale.ResourceSample, cpu hostCPUObservation) autoscale.MachineFacts {
		facts := machineFacts(slots[:listed], runtime.NumCPU(), host, addressSlots, c.slots == 0)
		facts.CopySlots = max(0, c.copySlots)
		if cpu.valid {
			facts.HostCPUPct, facts.HostCPUOK = cpu.busyPct, true
			facts.CPUPct = max(facts.CPUPct, cpu.busyPct)
		}
		if ready, known := autoscale.ReceiverOffer(facts); known || c.resourceSlots {
			facts.ReceiverReady = &ready
			facts.ReceiverRefusal = autoscale.ReceiverRefusal(facts)
		}
		return facts
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/spawn", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		o := c.defaults
		o.Genesis = q.Get("genesis") == "1"
		o.PreferNear = q.Get("prefer_near")
		if v := q.Get("compaction"); v != "" {
			o.Compaction = v == "true"
		}
		if v := q.Get("items"); v != "" {
			o.Items, _ = strconv.Atoi(v)
		}
		if q.Has("provisioner") {
			o.Provisioner = q.Get("provisioner")
		}
		if v := q.Get("bootstrap"); v != "" {
			o.Bootstrap = v
		}
		if !o.Genesis && o.Bootstrap == "" {
			http.Error(w, "no bootstrap", http.StatusBadRequest)
			return
		}
		var host autoscale.ResourceSample
		var cpu hostCPUObservation
		if c.resourceSlots {
			host, cpu = readHost()
		}
		mu.Lock()
		if resetting || c.resourceSlots && offeredSlots(factsLocked(host, cpu)) == 0 {
			mu.Unlock()
			http.Error(w, "no receiver capacity on this machine", http.StatusServiceUnavailable)
			return
		}
		i := -1
		for j, s := range slots {
			if !alive(s) {
				i = j
				break
			}
		}
		if i < 0 && c.slots == 0 {
			i = len(slots)
		}
		var spawned *slot
		err := fmt.Errorf("no free slot")
		if i >= 0 {
			err = start(i, o, true)
			if err == nil {
				spawned = slots[i]
			}
		}
		mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if err := wait(spawned, q.Get("wait") == "ready" && !o.Genesis); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "local-%s-%d\n", c.host, i)
	})
	mux.HandleFunc("/restart", func(w http.ResponseWriter, r *http.Request) {
		i, err := slotArg(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s, err := crash(i)
		if err == nil {
			mu.Lock()
			if resetting || slots[i] != s {
				err = fmt.Errorf("slot changed during restart")
			} else {
				err = start(i, s.options, false)
			} // same disk: a genesis is not initialized again
			mu.Unlock()
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "restarted", i)
	})
	mux.HandleFunc("/kill", func(w http.ResponseWriter, r *http.Request) {
		i, err := slotArg(r)
		if err == nil {
			_, err = crash(i)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "killed", i)
	})
	mux.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if resetting {
			mu.Unlock()
			http.Error(w, "reset in progress", http.StatusConflict)
			return
		}
		resetting = true
		n := listed
		mu.Unlock()
		for i := 0; i < n; i++ {
			_, _ = crash(i)
		}
		mu.Lock()
		for i := range slots {
			slots[i] = nil
		}
		for i := 0; i < n; i++ {
			keepLog(i)
		}
		_ = os.RemoveAll(c.dir)
		if c.resourceSlots {
			listed = 0
		}
		resetting = false
		mu.Unlock()
		fmt.Fprintln(w, "reset")
	})
	mux.HandleFunc("/count", func(w http.ResponseWriter, r *http.Request) {
		if c.resourceSlots {
			host, cpu := readHost()
			mu.Lock()
			facts := factsLocked(host, cpu)
			mu.Unlock()
			fmt.Fprintf(w, "%d %d\n", facts.Running, offeredSlots(facts))
			return
		}
		mu.Lock()
		n := 0
		for _, s := range slots {
			if alive(s) {
				n++
			}
		}
		mu.Unlock()
		fmt.Fprintf(w, "%d %d\n", n, c.slots-n)
	})
	// observe reads every live node's /metrics: its name, whether it is
	// still joining and its CPU time. The node measures, the agent sums.
	observe := func() {
		mu.Lock()
		n := listed
		mu.Unlock()
		for i := 0; i < n; i++ {
			mu.Lock()
			s := slots[i]
			live := alive(s)
			mu.Unlock()
			if !live {
				continue
			}
			var m struct {
				Name    string `json:"name"`
				Joining bool   `json:"joining"`
				Demand  struct {
					Applied *int64 `json:"applied"`
				} `json:"demand"`
				Process struct {
					CPUSeconds float64 `json:"cpu_seconds"`
				} `json:"process"`
				Storage struct {
					Items        *int64 `json:"items"`
					ItemsCounted bool   `json:"items_counted"`
					ItemsAgeMS   int64  `json:"items_age_ms"`
				} `json:"storage"`
			}
			// One second: an old node does not answer while it joins, and
			// must not delay the reading of the other slots.
			call, cancel := context.WithTimeout(ctx, time.Second)
			req, _ := http.NewRequestWithContext(call, http.MethodGet, fmt.Sprintf("%s://127.0.0.1:%d/metrics", scheme, s.ports.monitor), nil)
			res, err := client.Do(req)
			if err == nil {
				err = json.NewDecoder(res.Body).Decode(&m)
				res.Body.Close()
			}
			cancel()
			if err != nil || res.StatusCode != http.StatusOK {
				continue
			}
			mu.Lock()
			s.record(m.Name, m.Joining, m.Process.CPUSeconds, m.Demand.Applied, time.Now())
			s.recordItems(m.Storage.Items, m.Storage.ItemsCounted, m.Storage.ItemsAgeMS, time.Now())
			mu.Unlock()
		}
	}
	go func() {
		for ctx.Err() == nil {
			observe()
			time.Sleep(factsEvery)
		}
	}()
	mux.HandleFunc("/profile", func(w http.ResponseWriter, r *http.Request) {
		relayProfile(w, r, c.debugBase > 0, func(i int) int {
			mu.Lock()
			defer mu.Unlock()
			if i >= len(slots) || slots[i] == nil {
				return 0
			}
			return slots[i].ports.debug
		}, "http://127.0.0.1")
	})
	mux.HandleFunc("/facts", func(w http.ResponseWriter, r *http.Request) {
		host, cpu := readHost()
		mu.Lock()
		facts := factsLocked(host, cpu)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(struct {
			autoscale.MachineFacts
			Sample hostCPUObservation `json:"host_cpu_sample"`
		}{facts, cpu})
	})
	mux.HandleFunc("/slots", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		out := []map[string]any{}
		for i, s := range slots[:listed] {
			if c.resourceSlots && s == nil {
				continue
			}
			ports := nodePorts{p2p: c.p2pBase + i, monitor: c.monBase + i}
			if s != nil {
				ports = s.ports
			}
			row := map[string]any{"slot": i, "p2p": fmt.Sprintf("%s://%s:%d", scheme, c.advertise, ports.p2p), "monitor": fmt.Sprintf("%s://%s:%d", scheme, c.advertise, ports.monitor)}
			if s != nil {
				row["alive"], row["started"], row["options"] = alive(s), s.started, s.options
				if !alive(s) {
					row["exited"], row["exit_code"] = s.exited, s.exitCode
				}
			}
			out = append(out, row)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: mux}
	if *idleExit > 0 {
		// Scale-down of machines: a machine whose vnodes all folded away is
		// released. One that never ran a node is a spare and stays.
		go func() {
			var idleSince time.Time
			for range time.Tick(time.Second) {
				mu.Lock()
				ran, live := false, 0
				for _, s := range slots {
					ran = ran || s != nil
					if alive(s) {
						live++
					}
				}
				mu.Unlock()
				switch {
				case !ran || live > 0:
					idleSince = time.Time{}
				case idleSince.IsZero():
					idleSince = time.Now()
				case time.Since(idleSince) >= *idleExit:
					log.Printf("agent: no live node for %s, releasing the machine", *idleExit)
					_ = server.Close()
					return
				}
			}
		}()
	}
	go func() { <-ctx.Done(); _ = server.Close() }()
	log.Printf("agent: listening on %s (resource admission=%t, fixed slots=%d)", *listen, c.resourceSlots, c.slots)
	err = server.Serve(listener)
	mu.Lock()
	for _, s := range slots {
		if alive(s) {
			_ = s.cmd.Process.Signal(os.Interrupt)
		}
	}
	mu.Unlock()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// profileSecondsMax bounds one CPU profile relayed by the agent.
const profileSecondsMax = 60

// relayProfile serves GET /profile?slot=I&seconds=N: the CPU profile of slot
// I's node, read from its loopback profiling port. The node only listens on
// loopback (internal/diagnostics), so the agent is the way to profile a
// receiver from another machine of the experiment. 404 when profiling is off.
func relayProfile(w http.ResponseWriter, r *http.Request, enabled bool, portFor func(int) int, origin string) {
	if !enabled {
		http.Error(w, "profiling is off: start the agent with -debug-base and a node built with -tags diagnostic", http.StatusNotFound)
		return
	}
	i, err := strconv.Atoi(r.URL.Query().Get("slot"))
	if err != nil || i < 0 {
		http.Error(w, "slot out of range", http.StatusBadRequest)
		return
	}
	port := portFor(i)
	if port == 0 {
		http.Error(w, "slot out of range", http.StatusBadRequest)
		return
	}
	seconds := 10
	if raw := r.URL.Query().Get("seconds"); raw != "" {
		if seconds, err = strconv.Atoi(raw); err != nil || seconds < 1 || seconds > profileSecondsMax {
			http.Error(w, fmt.Sprintf("seconds must be 1 to %d", profileSecondsMax), http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(seconds+10)*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s:%d/debug/pprof/profile?seconds=%d", origin, port, seconds), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}
