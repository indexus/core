package p2p

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalChildrenHTTPContract(t *testing.T) {
	h := NewHttpHandler("", &countingService{}, nil)
	const valid = "?collection=Demo000000000000&location=%40"
	for _, tc := range []struct {
		method, query string
		auth, cancel  bool
		status        int
	}{
		{"GET", valid, false, false, 200}, {"GET", valid, true, false, 401}, {"POST", valid, false, false, 405},
		{"GET", valid + "&deep=true", false, false, 400}, {"GET", valid + "&location=a", false, false, 400},
		{"GET", "?collection=c&location=%QQ", false, false, 400}, {"GET", "?location=a", false, false, 400},
		{"GET", valid, false, true, 503},
	} {
		h.RequireAuth = tc.auth
		r := httptest.NewRequest(tc.method, "/zones/children/local"+tc.query, nil)
		if tc.cancel {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			r = r.WithContext(ctx)
		}
		out := httptest.NewRecorder()
		h.server.Handler.ServeHTTP(out, r)
		if out.Code != tc.status || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s: %d %v", tc.method, tc.query, out.Code, out.Header())
		}
		if tc.status == http.StatusOK {
			var result struct {
				Children []string
				Complete *bool
			}
			if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil || result.Children == nil || len(result.Children) != 0 || result.Complete == nil || *result.Complete {
				t.Fatalf("invalid local observation: %s", out.Body)
			}
		}
	}
}
