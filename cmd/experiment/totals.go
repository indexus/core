package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
)

// collectionTotal follows shallow /set referrals. A null set names a route,
// not an empty collection; deep+refresh still asks for an exact durable total.
func collectionTotal(node, tok, collection string, deep bool) (int64, error) {
	return collectionTotalContext(context.Background(), node, tok, collection, deep)
}

func collectionTotalContext(ctx context.Context, node, tok, collection string, deep bool) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	seen := map[string]bool{}
	for range indexus.BASE64.IDLength() * 6 { // at most the address bit width
		node = strings.TrimRight(node, "/")
		if seen[node] {
			return 0, fmt.Errorf("total route cycle at %s", node)
		}
		seen[node] = true
		endpoint, err := url.Parse(node)
		if err != nil {
			return 0, err
		}
		query := url.Values{"location": {"@"}, "collection": {collection}, "deep": {strconv.FormatBool(deep)}, "refresh": {strconv.FormatBool(deep)}}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, node+"/set?"+query.Encode(), nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		var body struct {
			Error string `json:"error"`
			Set   map[string]struct {
				Count int64 `json:"count"`
			} `json:"set"`
			Contact struct {
				IP   string         `json:"ip"`
				IPs  map[string]any `json:"ips"`
				Port int            `json:"port"`
			} `json:"contact"`
		}
		err = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			detail := strings.Join(strings.Fields(body.Error), " ")
			if res.StatusCode == http.StatusServiceUnavailable && detail == indexus.ErrReadChanged.Error() {
				return 0, fmt.Errorf("status %d: %w", res.StatusCode, indexus.ErrReadChanged)
			}
			return 0, fmt.Errorf("status %d: %s", res.StatusCode, detail)
		}
		if err != nil {
			return 0, err
		}
		if body.Set != nil {
			var count int64
			for _, child := range body.Set {
				count += child.Count
			}
			return count, nil
		}
		ip := body.Contact.IP
		if ip == "" {
			var ips []string
			for k := range body.Contact.IPs {
				ips = append(ips, k)
			}
			sort.Strings(ips)
			if len(ips) > 0 {
				ip = ips[0]
			}
		}
		if ip == "" || body.Contact.Port < 1 || body.Contact.Port > 65535 {
			return 0, fmt.Errorf("null total without a usable route")
		}
		// Contacts may omit scheme; keep the entry's transport (especially TLS).
		node = endpoint.Scheme + "://" + net.JoinHostPort(ip, strconv.Itoa(body.Contact.Port))
	}
	return 0, fmt.Errorf("total route exceeded address width")
}
