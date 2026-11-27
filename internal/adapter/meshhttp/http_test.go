package meshhttp

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
)

func TestRefusalRetainsMembershipControlEnvelope(t *testing.T) {
	for _, refusal := range []error{storage.ErrMissingData, storage.ErrConflict, storage.ErrWrongCluster, causal.ErrConflict, causal.ErrDependency} {
		t.Run(refusal.Error(), func(t *testing.T) {
			server := httptest.NewServer(&Handler{Mesh: func(context.Context, mesh.Request) (mesh.Response, error) {
				return mesh.Response{Membership: &mesh.MembershipReply{Packet: mesh.MembershipPacket{Body: &mesh.MembershipEnvelope{Genesis: causal.Genesis{Cluster: "test"}}}}}, refusal
			}})
			defer server.Close()
			client := Client{}
			response, err := client.Exchange(context.Background(), server.URL, mesh.Request{Kind: "membership"})
			if !errors.Is(err, refusal) || response.Membership == nil || response.Membership.Packet.Body == nil || response.Membership.Packet.Body.Genesis.Cluster != "test" {
				t.Fatalf("lost refusal or membership: %v %+v", err, response)
			}
			if errors.Is(err, errors.New("unrelated")) {
				t.Fatal("unknown error matched refusal")
			}
		})
	}
}

// A missing metadata reference must reach the sender even on an RPC refusal;
// the sender retries with full facts only after this explicit pre-dispatch miss.
func TestMembershipReferenceMissSurvivesRPCRefusal(t *testing.T) {
	version := causal.Digest{1, 2, 3}
	server := httptest.NewServer(&Handler{Mesh: func(_ context.Context, request mesh.Request) (mesh.Response, error) {
		if request.Membership == nil || request.Membership.Version != version || request.Membership.Known != version || request.Membership.Body != nil {
			t.Error("metadata reference changed in transit")
		}
		return mesh.Response{Membership: &mesh.MembershipReply{NeedBody: true}}, causal.ErrDependency
	}})
	defer server.Close()
	response, err := (&Client{}).Exchange(context.Background(), server.URL, mesh.Request{Kind: "membership", Membership: &mesh.MembershipPacket{Version: version, Known: version}})
	if !errors.Is(err, causal.ErrDependency) || response.Membership == nil || !response.Membership.NeedBody {
		t.Fatalf("lost explicit cache miss: response=%+v err=%v", response, err)
	}
}

func TestMeshRefusalIsCountedAtBothEnds(t *testing.T) {
	const kind = "install-zone"
	receiver, sender := metrics.Handled(kind), metrics.Sent(kind)
	beforeReceiver, beforeSender := receiver.Errors.Load(), sender.Errors.Load()
	server := httptest.NewServer(&Handler{Mesh: func(context.Context, mesh.Request) (mesh.Response, error) {
		return mesh.Response{}, storage.ErrConflict
	}})
	defer server.Close()
	_, err := (&Client{}).Exchange(context.Background(), server.URL, mesh.Request{Kind: kind})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("received %v", err)
	}
	if receiver.Errors.Load()-beforeReceiver != 1 || sender.Errors.Load()-beforeSender != 1 {
		t.Fatal("both sides must count the same refused RPC")
	}
}
