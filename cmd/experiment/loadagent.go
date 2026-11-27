package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
)

// loadAgent lets one coordinator drive several load machines (LOAD-SYNC):
//
//	POST /run?step=NAME&arg=-rate&arg=900&arg=-start-at&arg=...   start `experiment load` with these flags
//	GET  /status                                                  "running" or "idle"
//	GET  /file?step=NAME&name=load.jsonl|acked.jsonl|load.log     a result file of a step
//
// The coordinator gives every machine the same -start-at and its share of the
// rate, so the machines climb each step together. It only ever runs this
// binary's own load command, in its own directory.
func loadAgent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("loadagent", flag.ExitOnError)
	listen := fs.String("listen", ":22200", "address")
	dir := fs.String("dir", "/var/tmp/indexus-load", "where steps write their results")
	_ = fs.Parse(args)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var running *exec.Cmd
	stepName := regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)
	files := map[string]bool{"load.jsonl": true, "acked.jsonl": true, "load.log": true, "stacks.txt": true}
	mux := http.NewServeMux()
	mux.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		step := q.Get("step")
		if r.Method != http.MethodPost || !stepName.MatchString(step) {
			http.Error(w, "POST /run?step=NAME&arg=...", http.StatusBadRequest)
			return
		}
		out := filepath.Join(*dir, step)
		if err := os.MkdirAll(out, 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if running != nil {
			http.Error(w, "a step is already running", http.StatusConflict)
			return
		}
		logf, err := os.Create(filepath.Join(out, "load.log"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cmd := exec.Command(self, append(append([]string{"load"}, q["arg"]...), "-out", out)...)
		cmd.Stdout, cmd.Stderr = logf, logf
		if err := cmd.Start(); err != nil {
			logf.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		running = cmd
		go func() {
			_ = cmd.Wait()
			logf.Close()
			mu.Lock()
			running = nil
			mu.Unlock()
		}()
		fmt.Fprintln(w, "started", step)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		busy := running != nil
		mu.Unlock()
		if busy {
			fmt.Fprintln(w, "running")
		} else {
			fmt.Fprintln(w, "idle")
		}
	})
	mux.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) {
		step, name := r.URL.Query().Get("step"), r.URL.Query().Get("name")
		if !stepName.MatchString(step) || !files[name] {
			http.Error(w, "bad step or name", http.StatusBadRequest)
			return
		}
		http.ServeFile(w, r, filepath.Join(*dir, step, name))
	})
	server := &http.Server{Addr: *listen, Handler: mux}
	go func() { <-ctx.Done(); _ = server.Close() }()
	log.Printf("loadagent: %s, results in %s", *listen, *dir)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
