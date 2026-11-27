package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxEntries bounds the entry points a loader learns, and the slow_by_node
// counters indexed by them.
const maxEntries = 1024

// entries are the nodes a loader writes through. A real client enters the
// network anywhere and learns peers from it; a loader that only knew its
// seeds kept every write on them (one gateway for the whole spike, on the
// node that was also hot and splitting). With discovery on, the list grows
// with the network: the loader asks a known node for its neighbours, as
// peers do, and drops a node it cannot reach until it answers again.
//
// The index of a URL never changes, so slow_by_node stays comparable.
type entries struct {
	mu    sync.Mutex
	urls  []string
	names []string
	alive []bool
	snap  []int // indices of alive entries
}

func newEntries(seeds []string) *entries {
	e := &entries{}
	for _, s := range seeds {
		e.add(s)
	}
	return e
}

func (e *entries) add(u string) bool {
	for _, known := range e.urls {
		if known == u {
			return false
		}
	}
	if len(e.urls) >= maxEntries {
		return false
	}
	e.urls = append(e.urls, u)
	e.names = append(e.names, "")
	e.alive = append(e.alive, true)
	e.refresh()
	return true
}

func (e *entries) refresh() {
	e.snap = e.snap[:0]
	for i, ok := range e.alive {
		if ok {
			e.snap = append(e.snap, i)
		}
	}
}

// pick returns a random alive entry and its stable index.
func (e *entries) pick(rng *rand.Rand) (int, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.snap) == 0 {
		i := rng.IntN(len(e.urls))
		return i, e.urls[i]
	}
	i := e.snap[rng.IntN(len(e.snap))]
	return i, e.urls[i]
}

// down marks an entry unreachable; the last alive one is kept, so a loader
// never runs out of entry points because of one transient error.
func (e *entries) down(i int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.alive[i] && len(e.snap) > 1 {
		e.alive[i] = false
		e.refresh()
	}
}

func (e *entries) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.snap)
}

func (e *entries) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.urls...)
}

// discover learns entry points every period until ctx ends: it asks one
// alive entry for its neighbours, adds the new ones, and pings the entries
// marked down so that a node back up is used again.
func (e *entries) discover(ctx context.Context, tok string, every time.Duration, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 7000))
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		i, u := e.pick(rng)
		name, err := e.name(i, u, tok)
		if err != nil {
			e.down(i)
			continue
		}
		for _, c := range neighbours(u, tok, name) {
			if c != "" && ping(c, tok) == nil {
				e.mu.Lock()
				e.add(c)
				e.mu.Unlock()
			}
		}
		e.mu.Lock()
		dead := map[int]string{}
		for j, ok := range e.alive {
			if !ok {
				dead[j] = e.urls[j]
			}
		}
		e.mu.Unlock()
		for j, du := range dead {
			if ping(du, tok) == nil {
				e.mu.Lock()
				e.alive[j] = true
				e.refresh()
				e.mu.Unlock()
			}
		}
	}
}

// name is the vnode name an entry answers /ping with, cached once known.
func (e *entries) name(i int, u, tok string) (string, error) {
	e.mu.Lock()
	n := e.names[i]
	e.mu.Unlock()
	if n != "" {
		return n, nil
	}
	req, _ := http.NewRequest(http.MethodPost, u+"/ping", bytes.NewBufferString(`{"origin":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var body struct {
		Contact struct {
			Name string `json:"name"`
		} `json:"contact"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&body) != nil || body.Contact.Name == "" {
		return "", fmt.Errorf("ping %s: %d", u, res.StatusCode)
	}
	e.mu.Lock()
	e.names[i] = body.Contact.Name
	e.mu.Unlock()
	return body.Contact.Name, nil
}

// neighbours asks u for the contacts near origin, as URLs in the scheme of
// u. A contact's address is its IP field, else any of its IPs.
func neighbours(u, tok, origin string) []string {
	req, _ := http.NewRequest(http.MethodGet, u+"/neighbors?origin="+url.QueryEscape(origin), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	var body struct {
		Neighbors []struct {
			IPs  map[string]any `json:"ips"`
			Port int            `json:"port"`
			IP   string         `json:"ip"`
		} `json:"neighbors"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&body) != nil {
		return nil
	}
	scheme := "http"
	if strings.HasPrefix(u, "https://") {
		scheme = "https"
	}
	var out []string
	for _, c := range body.Neighbors {
		ip := c.IP
		if ip == "" {
			for k := range c.IPs {
				ip = k
				break
			}
		}
		if ip == "" || c.Port == 0 {
			continue
		}
		if strings.Contains(ip, ":") {
			ip = "[" + ip + "]"
		}
		out = append(out, fmt.Sprintf("%s://%s:%d", scheme, ip, c.Port))
	}
	return out
}

func ping(u, tok string) error {
	req, _ := http.NewRequest(http.MethodHead, u+"/ping", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return nil
}

// index is the stable index of u, added first if it is new (an owner a
// route named); -1 when maxEntries are known already.
func (e *entries) index(u string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.add(u)
	for i, known := range e.urls {
		if known == u {
			return i
		}
	}
	return -1
}

func (e *entries) url(rng *rand.Rand) string {
	_, u := e.pick(rng)
	return u
}
