package p2p

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

type fingerprintService struct {
	countingService
	token string
	err   error
	calls int
}

func (s *fingerprintService) ReadLocalFingerprintContext(ctx context.Context, _, _ string) (indexus.SubtreeFingerprint, error) {
	s.calls++
	if err := ctx.Err(); err != nil {
		return indexus.SubtreeFingerprint{}, err
	}
	if s.token == "" {
		return indexus.SubtreeFingerprint{}, s.err
	}
	return indexus.SubtreeFingerprint{Fingerprint: s.token, Guard: "opaque-guard", Sum: [32]byte{0xff}, Count: ^uint64(0)}, s.err
}

func TestLocalFingerprintHTTPContract(t *testing.T) {
	const query = "?collection=Demo000000000000&location=%40"
	for _, tc := range []struct {
		name, method, query, token string
		auth, cancel, unsupported  bool
		err                        error
		status, calls              int
	}{
		{name: "available", method: "GET", query: query, token: "opaque", status: 200, calls: 1},
		{name: "unknown", method: "GET", query: query, status: 200, calls: 1},
		{name: "service unavailable", method: "GET", query: query, err: indexus.ErrNotReady, status: 503, calls: 1},
		{name: "unknown capability", method: "GET", query: query, unsupported: true, status: 503},
		{name: "invalid key", method: "GET", query: query, err: indexus.ErrInvalidKey, status: 400, calls: 1},
		{name: "cancelled", method: "GET", query: query, cancel: true, status: 503, calls: 1},
		{name: "read scope required", method: "GET", query: query, auth: true, status: 401},
		{name: "method", method: "POST", query: query, status: 405},
		{name: "multiple locations", method: "GET", query: query + "&location=x", status: 400},
		{name: "missing collection", method: "GET", query: "?location=x", status: 400},
		{name: "bad escape", method: "GET", query: "?location=%QQ&collection=c", status: 400},
		{name: "network forbidden", method: "GET", query: query + "&deep=true", status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fingerprintService{token: tc.token, err: tc.err}
			var svc Service = s
			if tc.unsupported {
				svc = &countingService{}
			}
			h := NewHttpHandler("", svc, nil)
			h.RequireAuth = tc.auth
			req := httptest.NewRequest(tc.method, "/zones/fingerprint/local"+tc.query, nil)
			if tc.cancel {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			res := httptest.NewRecorder()
			h.server.Handler.ServeHTTP(res, req)
			if res.Code != tc.status || s.calls != tc.calls || res.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("response %d, calls %d, headers %v", res.Code, s.calls, res.Header())
			}
			if tc.status != 200 {
				return
			}
			var body map[string]any
			if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			value, present := body["fingerprint"]
			guard, hasGuard := body["guard"]
			monoid, hasMonoid := body["monoid"]
			if !present || !hasGuard || !hasMonoid || body["collection"] != "Demo000000000000" || body["location"] != "@" || body["complete"] != false {
				t.Fatalf("unexpected %v", body)
			}
			if tc.token == "" {
				if value != nil || guard != nil || monoid != nil {
					t.Fatalf("unexpected known observation %v", body)
				}
			} else {
				m, ok := monoid.(map[string]any)
				if value != tc.token || guard != "opaque-guard" || !ok || m["algorithm"] != "sha256-add256-v1" || m["count"] != "18446744073709551615" || m["sum"] != "ff"+strings.Repeat("00", 31) {
					t.Fatalf("invalid monoid %v", body)
				}
			}
		})
	}
}
