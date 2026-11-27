package autoscale

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestScalePolicyProvisioningTimeoutIsBounded(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release }))
	defer server.Close()
	defer close(release)
	a := New(Config{Enabled: true, ProvisionTimeout: 150 * time.Millisecond})
	start := time.Now()
	err := a.PostScale(server.URL, "source", 1, 1, "target", "items", 1, []string{"target"})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("provisioner error: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("provisioner deadline was not bounded")
	}
	select {
	case <-started:
	default:
		t.Fatal("request never reached provisioner")
	}
}

func TestScalePolicyProvisioningBodyTimeoutIsAnError(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer server.Close()
	defer close(release)
	a := New(Config{Enabled: true, ProvisionTimeout: 150 * time.Millisecond})
	err := a.PostScale(server.URL, "source", 1, 1, "target", "items", 1, []string{"target"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("body timeout was reported as successful provisioning: %v", err)
	}
	if a.ClientWritesBlocked() {
		t.Fatal("provisioner timeout blocks writes")
	}
}

func TestScalePolicyOnlyHardMemoryPressureBlocksAdmission(t *testing.T) {
	a := New(Config{Enabled: true, MemRefusePct: 90})
	a.upInFlight.Store(true)
	a.lastUpAt = time.Now()
	a.risingFast = true
	if a.ClientWritesBlocked() {
		t.Fatal("control plane or rising signal blocks available memory")
	}
	a.mu.Lock()
	a.lastPressure["mem_pct"] = 95.0
	a.mu.Unlock()
	if !a.ClientWritesBlocked() {
		t.Fatal("measured memory exhaustion is ignored")
	}
	a.mu.Lock()
	a.lastPressure["mem_pct"] = 20.0
	a.mu.Unlock()
	if a.ClientWritesBlocked() {
		t.Fatal("memory recovery remains blocked by in-flight provisioner")
	}
}

func TestScaleReconsidersPressureOnlyAfterIssuerCooldown(t *testing.T) {
	now := time.Unix(100, 0)
	a := New(Config{Enabled: true, Now: func() time.Time { return now }})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"api_cooldown","retry_after_ms":2750}`))
	}))
	defer server.Close()
	a.pendingSince = now
	a.requestScaleUp(Request{}, func(req Request) error {
		return a.PostRequest(server.URL, "donor", 10, 2, req)
	})
	if !a.pendingSince.IsZero() || a.reserveRequest(now.Add(2749*time.Millisecond)) ||
		!a.reserveRequest(now.Add(2750*time.Millisecond)) || a.ClientWritesBlocked() {
		t.Fatal("an explicit retry deadline should suppress only new scale requests until it expires")
	}
}

func TestOverlappingTicksReserveOnePlanner(t *testing.T) {
	a := New(Config{Enabled: true, ItemsOnly: true, ItemsLimit: 1})
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	defer close(release)
	a.SetNearBuilder(func(string, int, string) []string {
		started <- struct{}{}
		<-release
		return nil
	})
	input := Input{OwnedItems: 2, SelfName: "donor"}
	go a.Tick(input, nil)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first tick never reached the planner")
	}
	done := make(chan struct{})
	go func() { a.Tick(input, nil); close(done) }()
	select {
	case <-done:
	case <-started:
		t.Fatal("both ticks entered the planner")
	case <-time.After(time.Second):
		t.Fatal("second tick blocked on the first one's plan")
	}
}
