// Local qualification only: observe real Linux counters and record scale requests.
// It never grants authority or provisions resources.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("listen", ":22200", "diagnostic endpoint")
	flag.Parse()
	runtime.GOMAXPROCS(runtime.NumCPU())
	var mu sync.Mutex
	var requests []json.RawMessage
	var stressing atomic.Bool
	http.HandleFunc("/v1/scale", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || !json.Valid(raw) {
			http.Error(w, "invalid JSON", 400)
			return
		}
		mu.Lock()
		requests = append(requests, json.RawMessage(raw))
		mu.Unlock()
		http.Error(w, "diagnostic recorder: no provisioning", 503)
	})
	http.HandleFunc("/snapshot", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]any{"requests": requests, "cpus": runtime.NumCPU(), "stress_active": stressing.Load()}
		for _, p := range []string{"/proc/stat", "/proc/locks", "/proc/meminfo", "/sys/fs/cgroup/cpu.max", "/sys/fs/cgroup/cpu.stat", "/sys/fs/cgroup/memory.current"} {
			b, e := os.ReadFile(p)
			if e == nil {
				out[p] = string(b)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	http.HandleFunc("/stress", func(w http.ResponseWriter, r *http.Request) {
		if !stressing.CompareAndSwap(false, true) {
			http.Error(w, "already running", 409)
			return
		}
		// Six of the Docker VM's eight cores for 40s. The orchestrator checks
		// the machine size first; no quota or CPU threshold is changed.
		go func() {
			var wg sync.WaitGroup
			until := time.Now().Add(40 * time.Second)
			for range 6 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for time.Now().Before(until) {
						for range 10000 {
						}
					}
				}()
			}
			wg.Wait()
			stressing.Store(false)
		}()
		w.WriteHeader(202)
	})
	if err := http.ListenAndServe(*addr, nil); err != nil {
		panic(err)
	}
}
