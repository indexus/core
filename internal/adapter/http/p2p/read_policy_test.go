package p2p

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
)

type policyReadService struct {
	*countingService
	options indexus.ReadOptions
	seen    context.Context
}

func (s *policyReadService) ReadContext(ctx context.Context, _, _ string, options indexus.ReadOptions) (indexus.ReadResult, error) {
	s.options, s.seen = options, ctx
	return indexus.ReadResult{Set: wire.NewSet(), Source: "cache", ObservedAt: time.Unix(1234, 0), FallbackError: "first failure\nsecond failure"}, ctx.Err()
}
func (s *policyReadService) GetSetsBlockContext(ctx context.Context, _ string, _ []string, _ bool, _ indexus.Visited, _ bool, options *indexus.ReadOptions, _ wire.SetsBlockMetrics, _ []int) ([]byte, error) {
	s.options, s.seen = *options, ctx
	return nil, ctx.Err()
}
func TestHTTPReadPoliciesAreExplicitAndPropagateContexts(t *testing.T) {
	for _, path := range []string{"/set", "/sets"} {
		service := &policyReadService{countingService: &countingService{}}
		h := &Handler{Service: service}
		req := httptest.NewRequest(http.MethodGet, path+"?collection=demo&location=a&read_mode=cached&max_age=1m", nil)
		out := httptest.NewRecorder()
		if path == "/sets" {
			h.GetSets(out, req)
		} else {
			h.GetSet(out, req)
		}
		if out.Code != 200 || service.options.Mode != indexus.ReadCached || service.options.MaxAge != time.Minute || service.seen != req.Context() {
			t.Fatalf("%s policy/context lost: %d %+v", path, out.Code, service.options)
		}
		if path == "/set" && (out.Header().Get("X-Indexus-Read-Source") != "cache" || out.Header().Get("X-Indexus-Observed-At") == "" || out.Header().Get("X-Indexus-Read-Fallback") != "true") {
			t.Fatal("cache provenance hidden")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		out = httptest.NewRecorder()
		req = req.WithContext(ctx)
		if path == "/sets" {
			h.GetSets(out, req)
		} else {
			h.GetSet(out, req)
		}
		if out.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s hid cancellation: %d", path, out.Code)
		}
	}
}
func TestHTTPRejectsInvalidReadPoliciesBeforeDispatch(t *testing.T) {
	for _, query := range []string{"read_mode=unknown", "read_mode=cached", "read_mode=cached&max_age=-1s", "read_mode=stale-if-error&max_age=bad"} {
		req := httptest.NewRequest(http.MethodGet, "/set?collection=demo&location=a&"+query, nil)
		out := httptest.NewRecorder()
		h := &Handler{Service: &countingService{}}
		h.GetSet(out, req)
		if out.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", query, out.Code)
		}
	}
}
