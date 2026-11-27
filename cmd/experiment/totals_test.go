package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func totalReferral(t *testing.T, destination string) map[string]any {
	t.Helper()
	u, err := url.Parse(destination)
	if err != nil {
		t.Fatal(err)
	}
	host, p, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"contact": map[string]any{"ips": map[string]any{host: nil}, "port": port}, "set": nil}
}

func TestCollectionTotalFollowsShallowReferral(t *testing.T) {
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-test" || r.URL.Query().Get("collection") != "a+b" {
			t.Error("referral lost token or query")
		}
		if r.URL.Query().Get("refresh") != "false" {
			t.Error("shallow read silently became exact")
		}
		_, _ = w.Write([]byte(`{"set":{"a":{"count":7},"b":{"count":3}}}`))
	}))
	defer owner.Close()
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(totalReferral(t, owner.URL))
	}))
	defer entry.Close()
	got, err := collectionTotal(entry.URL, "local-test", "a+b", false)
	if err != nil || got != 10 {
		t.Fatalf("got %d: %v", got, err)
	}
}

func TestCollectionTotalRejectsReferralCycleAndMissingRoute(t *testing.T) {
	for _, cycle := range []bool{false, true} {
		var endpoint string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cycle {
				_ = json.NewEncoder(w).Encode(totalReferral(t, endpoint))
			} else {
				_, _ = w.Write([]byte(`{"set":null}`))
			}
		}))
		endpoint = server.URL
		_, err := collectionTotal(endpoint, "test", "Demo", false)
		server.Close()
		if err == nil {
			t.Fatalf("cycle=%v: unavailable total became zero", cycle)
		}
	}
}

func TestCollectionTotalKeepsEmptyExactResultAndErrors(t *testing.T) {
	for _, status := range []int{200, 503} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("deep") != "true" || r.URL.Query().Get("refresh") != "true" {
				t.Error("exact flags lost")
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"set":{}}`))
		}))
		got, err := collectionTotal(server.URL, "test", "Demo", true)
		server.Close()
		if status == 200 && (err != nil || got != 0) {
			t.Fatalf("empty exact: %d %v", got, err)
		}
		if status == 503 && err == nil {
			t.Fatal("503 counted as empty")
		}
	}
}

func TestCollectionTotalPreservesTLSAcrossReferral(t *testing.T) {
	owner := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"set":{"a":{"count":9}}}`)) }))
	defer owner.Close()
	entry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(totalReferral(t, owner.URL))
	}))
	defer entry.Close()
	previous := client
	client = owner.Client()
	defer func() { client = previous }()
	got, err := collectionTotal(entry.URL, "test", "Demo", false)
	if err != nil || got != 9 {
		t.Fatalf("TLS referral: %d %v", got, err)
	}
}
