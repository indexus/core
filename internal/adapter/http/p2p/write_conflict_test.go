package p2p

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestWriteConflictIsPermanentAndUnavailableRemainsRetryable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		retry  string
	}{
		{"committed-conflict", fmt.Errorf("replay: %w", indexus.ErrConflict), http.StatusConflict, ""},
		{"not-ready", indexus.ErrNotReady, http.StatusServiceUnavailable, "1"},
		{"success", nil, http.StatusCreated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			out := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/item", strings.NewReader(`{"item":{"collection":"demo","location":"a","id":"x"},"root":"@"}`))
			h.writeItem(out, r, func(*indexus.Item, string, indexus.Visited) error { return tc.err })
			if out.Code != tc.status || out.Header().Get("Retry-After") != tc.retry {
				t.Fatalf("status=%d retry=%q", out.Code, out.Header().Get("Retry-After"))
			}
		})
	}
}
