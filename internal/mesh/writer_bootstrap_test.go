package mesh

import (
	"context"
	"errors"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
)

type emptyKnowledgeTransport struct{ fallback Transport }

func (p emptyKnowledgeTransport) Exchange(ctx context.Context, host string, req Request) (Response, error) {
	if host == "empty" {
		return Response{}, nil
	}
	return p.fallback.Exchange(ctx, host, req)
}

func TestWriterRequiresBootstrapKnowledge(t *testing.T) {
	t.Parallel()
	w, err := NewWriter(context.Background(), emptyKnowledgeTransport{}, []string{"empty"})
	if w != nil || !errors.Is(err, causal.ErrDependency) {
		t.Fatalf("empty bootstrap: writer=%v err=%v", w, err)
	}
}

func TestWriterSkipsEmptyBootstrapForValidSeed(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 1)
	w, err := NewWriter(context.Background(), emptyKnowledgeTransport{net}, []string{"empty", nodes[0].Self.Host})
	if err != nil {
		t.Fatal(err)
	}
	if w.Stats().Active != 1 {
		t.Fatalf("valid seed did not supply topology: %+v", w.Stats())
	}
}
