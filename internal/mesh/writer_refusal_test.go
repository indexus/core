package mesh

import (
	"context"
	"errors"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
)

// Preserve a server refusal when its validated metadata bundle is already
// known. Omitting that repeated bundle must not turn capacity into unavailability.
type versionRefusalTransport struct {
	fallback Transport
	refusal  error
	writes   int
	response Response
}

func (p *versionRefusalTransport) Exchange(ctx context.Context, host string, request Request) (Response, error) {
	if request.Kind != "write" {
		return p.fallback.Exchange(ctx, host, request)
	}
	p.writes++
	response := Response{}
	p.response = response
	return response, p.refusal
}

func TestWriterPreservesVersionOnlyServerRefusal(t *testing.T) {
	t.Parallel()
	for _, refusal := range []error{storage.ErrDataCapacity, errors.New("server admission refused")} {
		t.Run(refusal.Error(), func(t *testing.T) {
			net, nodes := meshFixture(t, 1)
			peers := &versionRefusalTransport{fallback: net, refusal: refusal}
			writer, err := NewWriter(context.Background(), peers, []string{nodes[0].Self.Host})
			if err != nil {
				t.Fatal(err)
			}
			record := storage.Record{Collection: "Collection000000", Location: "aa", ID: "refused"}
			_, err = writer.Write(context.Background(), "request-refused", record)
			if !errors.Is(err, refusal) {
				t.Fatalf("server refusal masked by routing retries: %v", err)
			}
			if peers.response.Membership != nil {
				t.Fatal("refusal carried membership")
			}
			if peers.writes != 1 {
				t.Fatalf("server refusal retried %d times", peers.writes)
			}
		})
	}
}
