package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func TestReadPointFollowsStaleACKToExactHolder(t *testing.T) {
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/records/direct" || r.URL.Query().Get("location") != "aa" {
			t.Errorf("unexpected read URL %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing authorization")
		}
		w.Write([]byte(`{"records":[{"id":"matched"}]}`))
	}))
	defer holder.Close()
	u, err := url.Parse(holder.URL)
	if err != nil {
		t.Fatal(err)
	}
	ip, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/records/direct" {
			t.Errorf("unexpected read URL %s", r.URL)
		}
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"contact": map[string]any{"ip": ip, "port": port}})
	}))
	defer old.Close()
	k := acknowledgedKey{key: key{Collection: "Demo000000000000", Location: "aa", ID: "matched"}, owner: old.URL}
	found, status, err := readPoint(context.Background(), k, "token", func() string {
		t.Error("a pair gave a usable next hop")
		return ""
	})
	if err != nil || !found || status != http.StatusOK {
		t.Fatalf("found %v, status %d, error %v", found, status, err)
	}
}
