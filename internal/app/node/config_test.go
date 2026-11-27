package node

import (
	"bytes"
	"encoding/json"
	"github.com/indexus/go-indexus-core/internal/adapter/peer"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func configurationFile(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	document := map[string]any{
		"storage":  map[string]any{"path": "data/backup"},
		"identity": map[string]any{"name": "AAAAAAAAAAAAAAAB", "network": "test-network", "cluster": "test-cluster", "issuer": "http://127.0.0.1:22000"},
		"network":  map[string]any{"listen": "127.0.0.1:21000", "monitoring": "127.0.0.1:19000", "advertise": []string{"127.0.0.1"}},
	}
	if mutate != nil {
		mutate(document)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestConfigurationHasOneSourceAndPreservesDistinctIdentities(t *testing.T) {
	t.Setenv("INDEXUS_CLUSTER", "wrong")
	t.Setenv("INDEXUS_MEM_REFUSE_PCT", "1")
	t.Setenv("INDEXUS_TOPOLOGY", "http://wrong:21000")
	path := configurationFile(t, nil)
	config, err := parseConfig([]string{"serve", "-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if !config.RequireAuth || config.CreateCluster || config.InitializeOnly || config.ClusterID != "test-cluster" || config.NetworkID != "test-network" || config.MemoryRefuse != 60 || len(config.Bootstraps) != 0 {
		t.Fatalf("incorrect production config: %+v", config)
	}
	if config.Storage != filepath.Join(filepath.Dir(path), "data/backup") || config.NodeKeyPath != filepath.Join(filepath.Dir(path), "data/node.ed25519") {
		t.Fatal("paths depend on working directory")
	}
	if config.StorageMemory != 0 {
		t.Fatalf("storage memory without the setting: %d", config.StorageMemory)
	}
	sized, err := parseConfig([]string{"serve", "-config", configurationFile(t, func(m map[string]any) {
		m["storage"].(map[string]any)["memory_bytes"] = 512 << 20
	})})
	if err != nil || sized.StorageMemory != 512<<20 {
		t.Fatalf("storage.memory_bytes: %d %v", sized.StorageMemory, err)
	}
	init, err := parseConfig([]string{"init", "-config", path})
	if err != nil || !init.CreateCluster || !init.InitializeOnly {
		t.Fatalf("init: %+v %v", init, err)
	}
	move, err := parseConfig([]string{"move", "-config", path, "-from", "https://127.0.0.1:21001"})
	if err != nil || move.MoveFrom == "" || move.CreateCluster {
		t.Fatalf("move: %+v %v", move, err)
	}
}

// The copy budget is off unless its directory is configured; the directory
// is resolved like every other path, so the vnodes of a host can share it.
func TestCopyBudgetIsOffUnlessItsDirectoryIsConfigured(t *testing.T) {
	config, err := loadConfig(configurationFile(t, nil))
	if err != nil || config.CopyBudgetDir != "" || config.CopySlots != 0 {
		t.Fatalf("default: %q %d %v", config.CopyBudgetDir, config.CopySlots, err)
	}
	path := configurationFile(t, func(m map[string]any) {
		m["limits"] = map[string]any{"copy_budget_directory": "../copy-budget", "copy_slots": 2}
	})
	config, err = loadConfig(path)
	if err != nil || config.CopyBudgetDir != filepath.Join(filepath.Dir(filepath.Dir(path)), "copy-budget") || config.CopySlots != 2 {
		t.Fatalf("configured: %q %d %v", config.CopyBudgetDir, config.CopySlots, err)
	}
}
func TestInvalidConfigurationFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"unknown knob":       func(m map[string]any) { m["delegation"] = 5000 },
		"no trust":           func(m map[string]any) { delete(m["identity"].(map[string]any), "issuer") },
		"negative budget":    func(m map[string]any) { m["limits"] = map[string]any{"data_bytes": -1} },
		"unsafe memory":      func(m map[string]any) { m["limits"] = map[string]any{"memory_refuse_pct": 101} },
		"invalid CPU target": func(m map[string]any) { m["scaling"] = map[string]any{"cpu_target_pct": 101} },
		"negative copies":    func(m map[string]any) { m["limits"] = map[string]any{"copy_slots": -1} },
		"tiny store":         func(m map[string]any) { m["storage"].(map[string]any)["memory_bytes"] = 1 << 20 },
		"bad listen":         func(m map[string]any) { m["network"].(map[string]any)["listen"] = "127.0.0.1:0" },
		"credentials in discovery": func(m map[string]any) {
			m["network"].(map[string]any)["bootstrap"] = []string{"http://secret@host:21000"}
		},
		"path in discovery": func(m map[string]any) {
			m["network"].(map[string]any)["bootstrap"] = []string{"http://host:21000/item"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfig(configurationFile(t, mutate)); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	path := configurationFile(t, nil)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(" {}")
	f.Close()
	if _, err := loadConfig(path); err == nil {
		t.Fatal("multiple JSON objects accepted")
	}
	for _, args := range [][]string{{"-delegation", "5000"}, {"serve", "-createCluster"}, {"move", "-config", configurationFile(t, nil)}, {"unknown"}} {
		if _, err := parseConfig(args); err == nil {
			t.Fatalf("invalid invocation accepted: %v", args)
		}
	}
}
func TestDiscoveryUsesTheSameOriginsForPeersAndAuthority(t *testing.T) {
	path := configurationFile(t, func(m map[string]any) {
		m["network"].(map[string]any)["bootstrap"] = []string{"https://[::1]:21005/", "http://127.0.0.1:21006"}
	})
	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Bootstraps) != 2 || config.Bootstraps[0].Port() != 21005 || !strings.Contains(config.TopologyEndpoints, "https://[::1]:21005") {
		t.Fatal("discovery disagrees with authority endpoints")
	}
	if _, err := parseConfig([]string{"init", "-config", path}); err == nil {
		t.Fatal("genesis accepted alongside bootstrap")
	}
}

// Compaction is on unless the file says "compaction": false (COLD-2).
func TestCompactionIsOnByDefault(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    any
		disabled bool
	}{{"absent", nil, false}, {"true", true, false}, {"false", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := loadConfig(configurationFile(t, func(d map[string]any) {
				if tc.value != nil {
					d["compaction"] = tc.value
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			if config.DisableCompaction != tc.disabled {
				t.Fatalf("DisableCompaction=%t want %t", config.DisableCompaction, tc.disabled)
			}
		})
	}
}

// SPAWN-1: prefer_near is a target. Two loads of the same file give two
// distinct names that both keep the target's first 64 bits.
func TestPreferNearGivesFreshNamesNearTheTarget(t *testing.T) {
	target := "AbCdEfGhIjKlMnOp"
	path := configurationFile(t, func(d map[string]any) {
		identity := d["identity"].(map[string]any)
		delete(identity, "name")
		identity["prefer_near"] = target
	})
	a, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name == b.Name {
		t.Fatalf("two spawns reused the identity %s", a.Name)
	}
	want, _ := indexus.BASE64.Decode(target)
	for _, name := range []string{a.Name, b.Name} {
		got, err := indexus.BASE64.Decode(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got[:8], want[:8]) {
			t.Fatalf("%s does not keep the first 64 bits of %s", name, target)
		}
	}
}

// Exercise the actual contact produced by config parsing, not just the stored
// bootstrap URL. This was HTTP even when TopologyEndpoints retained HTTPS.
func TestConfiguredHTTPSBootstrapProbe(t *testing.T) {
	var port int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping" || r.TLS == nil {
			t.Errorf("bootstrap probe %s TLS=%v", r.URL, r.TLS != nil)
		}
		json.NewEncoder(w).Encode(map[string]any{"contact": map[string]any{"name": "AAAAAAAAAAAAAAAB", "ips": map[string]any{"127.0.0.1": nil}, "port": port, "scheme": "https"}})
	}))
	defer server.Close()
	port = server.Listener.Addr().(*net.TCPAddr).Port
	previous := peer.HttpClient
	peer.HttpClient = server.Client()
	defer func() { peer.HttpClient = previous }()
	path := configurationFile(t, func(m map[string]any) { m["network"].(map[string]any)["bootstrap"] = []string{server.URL} })
	cfg, err := parseConfig([]string{"serve", "-config", path})
	if err != nil {
		t.Fatal(err)
	}
	contact, err := cfg.Bootstraps[0].Ping(cfg.Bootstraps[0])
	if err != nil {
		t.Fatal("HTTPS bootstrap used an HTTP contact", err)
	}
	if contact.Name() != "AAAAAAAAAAAAAAAB" {
		t.Fatal("wrong bootstrap identity")
	}
}

func TestSteadyCPUTargetFromNodeConfig(t *testing.T) {
	path := configurationFile(t, func(m map[string]any) {
		m["provisioner"] = "http://127.0.0.1:22001"
		m["scaling"] = map[string]any{"cpu_target_pct": 50, "writes_per_vnode": 100, "resource_signals": true}
	})
	c, err := loadConfig(path)
	if err != nil || c.Autoscale.CPULimitPct != 50 || c.Autoscale.WritesLimit != 100 || c.Autoscale.ItemsOnly {
		t.Fatalf("autoscale config: %+v, err %v", c.Autoscale, err)
	}
}
