package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// readPoint tries the last ACK's applier first. On a stale route it follows a
// peer's hint or tries another known entry. Each RPC reads only the named disk.
func readPoint(ctx context.Context, k acknowledgedKey, token string, entry func() string) (bool, int, error) {
	query := url.Values{"collection": {k.Collection}, "location": {k.Location}, "limit": {"1000"}}
	path := "/records/direct?" + query.Encode()
	node := k.owner
	tried := make(map[string]bool, 4)
	var status int
	var err error
	for range 4 {
		if node == "" || tried[node] {
			node = entry()
		}
		if node == "" || tried[node] {
			continue
		}
		tried[node] = true
		var found bool
		found, status, node, err = readPointAt(ctx, node, path, token, k.ID)
		if err == nil || ctx.Err() != nil || status >= 400 && status < 500 && status != http.StatusConflict && status != http.StatusTooManyRequests {
			return found, status, err
		}
	}
	if err == nil {
		err = fmt.Errorf("no peer for direct record read")
	}
	return false, status, err
}

func readPointAt(ctx context.Context, node, path, token, id string) (bool, int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(node, "/")+path, nil)
	if err != nil {
		return false, 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		return false, 0, "", err
	}
	defer res.Body.Close()
	var body struct {
		Records []struct{ ID string }
		Contact struct {
			IP   string
			Port int
		}
	}
	err = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != http.StatusOK {
		var next string
		if body.Contact.IP != "" && body.Contact.Port > 0 && body.Contact.Port <= 65535 {
			next = req.URL.Scheme + "://" + net.JoinHostPort(body.Contact.IP, strconv.Itoa(body.Contact.Port))
		}
		return false, res.StatusCode, next, fmt.Errorf("direct record read: status %d", res.StatusCode)
	}
	if err != nil {
		return false, res.StatusCode, "", err
	}
	if body.Records == nil {
		return false, res.StatusCode, "", fmt.Errorf("direct record reply lacks records")
	}
	for _, row := range body.Records {
		if row.ID == id {
			return true, res.StatusCode, "", nil
		}
	}
	return false, res.StatusCode, "", nil
}
