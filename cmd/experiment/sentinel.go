package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The sentinel watches a running experiment through every node's /metrics and
// stops it on the first sign the run is no longer telling us about scaling
// (the ladder's stop rules, see the indexus-scale-ladder skill). It reads only
// counters; it adds no load to the write path. On a stop it writes an incident
// directory with the reason and the last samples of every node, runs -on-stop
// (for example the cluster's teardown) and exits with status 3.

type sentinelRules struct {
	stagnation      float64       // acked/received below this ...
	stagnationFor   time.Duration // ... for this long
	minRate         float64       // only judged above this many received writes/s
	handoffOpen     time.Duration
	handoffLoop     int
	versionsFor     time.Duration
	membershipPerVN float64
	downAfter       int // consecutive failed polls of a node seen before
}

type nodeSample struct {
	At              time.Time      `json:"at"`
	URL             string         `json:"url"`
	Up              bool           `json:"up"`
	Received        int64          `json:"received"`
	Acked           int64          `json:"acked"`
	Refused         int64          `json:"refused"`
	Version         string         `json:"version"`
	VNodes          int64          `json:"known_vnodes"`
	MembershipBytes int64          `json:"membership_bytes"`
	Open            int64          `json:"handoffs_open"`
	Oldest          float64        `json:"handoff_oldest_seconds"`
	Attempts        int64          `json:"handoff_max_attempts"`
	CPU             float64        `json:"cpu_seconds"` // cumulative; the step summary turns it into cores
	Raw             map[string]any `json:"-"`
}

func sentinel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sentinel", flag.ExitOnError)
	monitors := fs.String("monitors", "", "comma-separated monitoring URLs")
	agents := fs.String("agents", "", "comma-separated agent URLs; their live slots are watched too (elastic runs)")
	every := fs.Duration("every", 2*time.Second, "poll period")
	duration := fs.Duration("duration", time.Hour, "watch at most this long")
	grace := fs.Duration("grace", 20*time.Second, "no stop before this")
	out := fs.String("out", "", "run directory; the incident goes to out/incident")
	onStop := fs.String("on-stop", "", "shell command run on a stop (teardown)")
	stacksAt := fs.Duration("stacks-at", 0, "when > 0, save every node's /debug/stacks once, that long after start (DIR/stacks/)")
	var r sentinelRules
	fs.Float64Var(&r.stagnation, "stagnation", 0.5, "stop when acked stays below this share of the writes not refused (stuck writes)")
	fs.DurationVar(&r.stagnationFor, "stagnation-for", 30*time.Second, "for this long")
	fs.Float64Var(&r.minRate, "min-rate", 5, "judge stagnation only above this many received writes/s")
	fs.DurationVar(&r.handoffOpen, "handoff-open", 2*time.Minute, "stop when a handoff stays open this long")
	fs.IntVar(&r.handoffLoop, "handoff-loop", 3, "stop when one operation is attempted this many times")
	fs.DurationVar(&r.versionsFor, "versions-for", time.Minute, "stop when membership versions differ this long with no handoff open")
	fs.Float64Var(&r.membershipPerVN, "membership-per-vnode", 4096, "stop when membership bytes exceed this times known vnodes")
	fs.IntVar(&r.downAfter, "down-after", 3, "stop when a node seen up fails this many polls in a row")
	_ = fs.Parse(args)
	if *out == "" {
		return errors.New("sentinel needs -out")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	log, err := os.Create(filepath.Join(*out, "sentinel.jsonl"))
	if err != nil {
		return err
	}
	defer log.Close()
	s := &sentinelState{rules: r, seen: map[string]int{}}
	start := time.Now()
	// Full /metrics of every node, first and latest poll: their difference gives
	// calls and service time per request kind and node, which says where the
	// time of a write goes (metrics-first.json, metrics-last.json).
	first := map[string]map[string]any{}
	ticker := time.NewTicker(*every)
	defer ticker.Stop()
	for {
		urls := split(*monitors)
		for _, a := range split(*agents) {
			urls = append(urls, s.agentMonitors(a)...)
		}
		samples := pollMetrics(dedupe(urls))
		last := map[string]map[string]any{}
		for _, x := range samples {
			line, _ := json.Marshal(x)
			fmt.Fprintln(log, string(line))
			if x.Up {
				last[x.URL] = x.Raw
				if _, ok := first[x.URL]; !ok {
					first[x.URL] = x.Raw
					saveJSON(filepath.Join(*out, "metrics-first.json"), first)
				}
			}
		}
		saveJSON(filepath.Join(*out, "metrics-last.json"), last)
		if *stacksAt > 0 && time.Since(start) >= *stacksAt {
			*stacksAt = 0
			go saveStacks(filepath.Join(*out, "stacks"), last)
		}
		reason := s.judge(samples)
		if reason != "" && time.Since(start) >= *grace {
			fmt.Fprintf(os.Stderr, "sentinel: STOP %s\n", reason)
			if err := s.incident(*out, reason, samples); err != nil {
				fmt.Fprintln(os.Stderr, "sentinel: incident:", err)
			}
			if *onStop != "" {
				cmd := exec.Command("sh", "-c", *onStop)
				cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
				_ = cmd.Run()
			}
			os.Exit(3)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if time.Since(start) > *duration {
			fmt.Fprintln(os.Stderr, "sentinel: run finished without a stop")
			return nil
		}
	}
}

type slotWatch struct {
	started   time.Time
	activated bool
}

type sentinelState struct {
	slots     map[string]slotWatch // process generation and observed installed/serving state
	rules     sentinelRules
	history   [][]nodeSample // last polls, newest last
	seen      map[string]int // url -> consecutive failures, present once seen up
	splitFrom time.Time      // first poll with differing versions
	lowFrom   time.Time      // first poll of the current low-ack window
}

// judge returns the first stop reason that holds, or "".
func (s *sentinelState) judge(now []nodeSample) string {
	s.history = append(s.history, now)
	if len(s.history) > 120 {
		s.history = s.history[1:]
	}
	r := s.rules
	at := time.Now()
	var reasons []string
	versions := map[string]bool{}
	open := false
	for _, x := range now {
		if !x.Up {
			if n, ok := s.seen[x.URL]; ok {
				s.seen[x.URL] = n + 1
				if n+1 >= r.downAfter {
					reasons = append(reasons, fmt.Sprintf("node_down %s (%d polls)", x.URL, n+1))
				}
			}
			continue
		}
		s.noteActivation(x.URL, x.Raw)
		s.seen[x.URL] = 0
		versions[x.Version] = true
		if x.Open > 0 {
			open = true
		}
		if x.Oldest > r.handoffOpen.Seconds() {
			reasons = append(reasons, fmt.Sprintf("handoff_open %s %.0fs", x.URL, x.Oldest))
		}
		if x.Attempts >= int64(r.handoffLoop) {
			reasons = append(reasons, fmt.Sprintf("handoff_loop %s %d attempts", x.URL, x.Attempts))
		}
		if x.VNodes > 0 && float64(x.MembershipBytes) > r.membershipPerVN*float64(x.VNodes) {
			reasons = append(reasons, fmt.Sprintf("membership_bloat %s %d bytes for %d vnodes", x.URL, x.MembershipBytes, x.VNodes))
		}
	}
	if len(versions) > 1 && !open {
		if s.splitFrom.IsZero() {
			s.splitFrom = at
		} else if at.Sub(s.splitFrom) > r.versionsFor {
			reasons = append(reasons, fmt.Sprintf("versions_split %d versions for %.0fs", len(versions), at.Sub(s.splitFrom).Seconds()))
		}
	} else {
		s.splitFrom = time.Time{}
	}
	// Stagnation over the cluster: acknowledged against received since the
	// previous poll, judged only while clients are writing.
	if len(s.history) >= 2 {
		prev := s.history[len(s.history)-2]
		received, acked, seconds := clusterDelta(prev, now)
		if seconds > 0 && float64(received)/seconds >= r.minRate && float64(acked) < r.stagnation*float64(received) {
			if s.lowFrom.IsZero() {
				s.lowFrom = at
			} else if at.Sub(s.lowFrom) >= r.stagnationFor {
				reasons = append(reasons, fmt.Sprintf("stagnation acked %d of %d received in the last poll, low for %.0fs", acked, received, at.Sub(s.lowFrom).Seconds()))
			}
		} else {
			s.lowFrom = time.Time{}
		}
	}
	sort.Strings(reasons)
	return strings.Join(reasons, "; ")
}

// saveStacks fetches each node's aggregated goroutine stacks, one file per node.
func saveStacks(dir string, nodes map[string]map[string]any) {
	_ = os.MkdirAll(dir, 0o755)
	i := 0
	for url := range nodes {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/debug/stacks", nil)
		if res, err := client.Do(req); err == nil {
			if f, err := os.Create(filepath.Join(dir, fmt.Sprintf("node-%02d.txt", i))); err == nil {
				fmt.Fprintf(f, "# %s\n", url)
				_, _ = io.Copy(f, res.Body)
				f.Close()
			}
			res.Body.Close()
		}
		cancel()
		i++
	}
}

func saveJSON(path string, v any) {
	raw, err := json.Marshal(v)
	if err == nil {
		_ = os.WriteFile(path+".tmp", raw, 0o644)
		_ = os.Rename(path+".tmp", path)
	}
}

func clusterDelta(prev, now []nodeSample) (received, acked int64, seconds float64) {
	before := map[string]nodeSample{}
	for _, x := range prev {
		if x.Up {
			before[x.URL] = x
		}
	}
	for _, x := range now {
		b, ok := before[x.URL]
		if !x.Up || !ok || x.Received < b.Received { // restarted: counters reset
			continue
		}
		// A refusal is an answer (overload is not stagnation): judge only the
		// writes that were not refused.
		received += (x.Received - b.Received) - (x.Refused - b.Refused)
		acked += x.Acked - b.Acked
		seconds = max(seconds, x.At.Sub(b.At).Seconds())
	}
	return
}

func (s *sentinelState) incident(out, reason string, last []nodeSample) error {
	dir := filepath.Join(out, "incident")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "reason.txt"), []byte(time.Now().Format(time.RFC3339)+" "+reason+"\n"), 0o644); err != nil {
		return err
	}
	for i, x := range last {
		if !x.Up {
			continue
		}
		raw, _ := json.MarshalIndent(x.Raw, "", " ")
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("metrics-%02d.json", i)), raw, 0o644)
	}
	history, _ := json.Marshal(s.history)
	return os.WriteFile(filepath.Join(dir, "history.json"), history, 0o644)
}

func pollMetrics(urls []string) []nodeSample {
	out := make([]nodeSample, len(urls))
	done := make(chan struct{}, len(urls))
	for i, u := range urls {
		go func() {
			defer func() { done <- struct{}{} }()
			out[i] = metricsSample(u)
		}()
	}
	for range urls {
		<-done
	}
	return out
}

func metricsSample(url string) nodeSample {
	x := nodeSample{At: time.Now(), URL: url}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var m map[string]any
	if err := getJSON(ctx, url+"/metrics", &m); err != nil {
		return x
	}
	x.Up, x.Raw = true, m
	num := func(path ...string) float64 {
		var v any = m
		for _, p := range path {
			o, _ := v.(map[string]any)
			v = o[p]
		}
		f, _ := v.(float64)
		return f
	}
	x.Received, x.Acked, x.Refused = int64(num("writes", "received")), int64(num("writes", "acknowledged")), int64(num("writes", "refused"))
	x.VNodes, x.MembershipBytes = int64(num("gossip", "known_vnodes")), int64(num("gossip", "membership_bytes"))
	x.CPU = num("process", "cpu_seconds")
	x.Open, x.Oldest, x.Attempts = int64(num("handoffs", "open", "count")), num("handoffs", "open", "oldest_seconds"), int64(num("handoffs", "open", "max_attempts"))
	if g, ok := m["gossip"].(map[string]any); ok {
		x.Version, _ = g["version"].(string)
	}
	return x
}

func getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("%s: status %d", url, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(v)
}

// agentMonitors lists the monitoring URLs of an agent's live slots.
func (s *sentinelState) agentMonitors(agent string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var slots []struct {
		Monitor  string     `json:"monitor"`
		Started  time.Time  `json:"started"`
		Exited   *time.Time `json:"exited"`
		ExitCode *int       `json:"exit_code"`
	}
	if err := getJSON(ctx, strings.TrimRight(agent, "/")+"/slots", &slots); err != nil {
		return nil
	}
	// Only an unactivated process generation may leave cleanly without an
	// alarm. A previously serving vnode stays watched even after exit 0.
	if s.slots == nil {
		s.slots = map[string]slotWatch{}
	}
	var out []string
	for _, slot := range slots {
		if slot.Monitor == "" {
			continue
		}
		watch := s.slots[slot.Monitor]
		if !watch.started.Equal(slot.Started) {
			watch = slotWatch{started: slot.Started}
			delete(s.seen, slot.Monitor)
		}
		s.slots[slot.Monitor] = watch
		if slot.Exited != nil && slot.ExitCode != nil && *slot.ExitCode == 0 && !watch.activated {
			continue
		}
		out = append(out, slot.Monitor)
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range in {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Activation is sticky for one process generation. An installed transfer
// or applied write is evidence even if client_ready was not sampled true.
func (s *sentinelState) noteActivation(url string, raw map[string]any) {
	active, _ := raw["client_ready"].(bool)
	for _, path := range [][2]string{{"handoffs", "taken"}, {"writes", "applied"}, {"demand", "applied"}} {
		bucket, _ := raw[path[0]].(map[string]any)
		value, _ := bucket[path[1]].(float64)
		active = active || value > 0
	}
	if active {
		if s.slots == nil {
			s.slots = map[string]slotWatch{}
		}
		watch := s.slots[url]
		watch.activated = true
		s.slots[url] = watch
	}
}
