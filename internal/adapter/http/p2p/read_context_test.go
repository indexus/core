package p2p

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
)

type contextualReadService struct {
	*countingService
	seen context.Context
}

func (s *contextualReadService) GetSetContext(ctx context.Context, _ string, _ string, _ bool, _ indexus.Visited, _ bool) (indexus.Contact, *indexus.Set, error) {
	s.seen = ctx
	return nil, nil, ctx.Err()
}
func (s *contextualReadService) GetSetsBlockContext(ctx context.Context, _ string, _ []string, _ bool, _ indexus.Visited, _ bool, _ *indexus.ReadOptions, _ wire.SetsBlockMetrics, _ []int) ([]byte, error) {
	s.seen = ctx
	return nil, ctx.Err()
}
func TestReadHandlersPropagateRequestCancellation(t *testing.T) {
	for _, path := range []string{"/set", "/sets"} {
		t.Run(path, func(t *testing.T) {
			service := &contextualReadService{countingService: &countingService{}}
			handler := &Handler{Service: service}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			request := httptest.NewRequest(http.MethodGet, path+"?collection=demo&location=aa", nil).WithContext(ctx)
			response := httptest.NewRecorder()
			if path == "/sets" {
				handler.GetSets(response, request)
			} else {
				handler.GetSet(response, request)
			}
			if service.seen != ctx || response.Code != http.StatusServiceUnavailable {
				t.Fatalf("request context lost: ctx=%v status=%d", service.seen, response.Code)
			}
		})
	}
}
