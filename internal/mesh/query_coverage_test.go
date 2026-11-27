package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// coverageJSONTransport deliberately serializes both DTOs. Passing Go pointers
// through MemoryNetwork cannot detect coverage/referrals accidentally json:"-".
type coverageJSONTransport struct {
	handle func(string, Request) (Response, error)
	mu     sync.Mutex
	calls  map[string]int
}

func (p *coverageJSONTransport) Exchange(ctx context.Context, host string, r Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return Response{}, err
	}
	var decoded Request
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return Response{}, err
	}
	p.mu.Lock()
	p.calls[host]++
	p.mu.Unlock()
	response, err := p.handle(host, decoded)
	if err != nil {
		return Response{}, err
	}
	raw, err = json.Marshal(response)
	if err != nil {
		return Response{}, err
	}
	var received Response
	err = json.Unmarshal(raw, &received)
	return received, err
}
func (p *coverageJSONTransport) count(host string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[host]
}
func coverageEndpoint(name string) causal.Endpoint {
	return causal.Endpoint{Owner: causal.Owner{VNode: name, Storage: "disk-" + name}, Host: name}
}
func coverageService(handle func(string, Request) (Response, error)) (*Service, *coverageJSONTransport) {
	wire := &coverageJSONTransport{handle: handle, calls: map[string]int{}}
	return &Service{Self: coverageEndpoint("entry"), Peers: wire}, wire
}
func coveragePage(regions []tree.Region, refs ...storage.RegionReferral) Response {
	return Response{Page: &storage.InventoryPage{Watermark: 1, Done: true, Coverage: regions, Referrals: refs}}
}

func TestQueryCoverageFailedHintCannotSatisfyMandatoryReferral(t *testing.T) {
	for _, position := range []string{"initial", "discovery"} {
		t.Run(position, func(t *testing.T) {
			unavailable := errors.New("receiver unavailable after ACK")
			source, receiver, empty := coverageEndpoint("source"), coverageEndpoint("receiver"), coverageEndpoint("empty")
			s, wire := coverageService(func(host string, r Request) (Response, error) {
				switch host {
				case receiver.Host:
					return Response{}, unavailable
				case empty.Host:
					return coveragePage(nil), nil
				case source.Host:
					result := coveragePage([]tree.Region{{}}, storage.RegionReferral{Region: tree.Region{}, Endpoint: receiver})
					result.Inventory = []storage.Snapshot{{Zone: tree.ZoneKey{Collection: "collection", Location: "@"}, Seq: 1, Records: []storage.Record{{ID: "old"}}}}
					return result, nil
				default:
					return Response{}, errors.New("unexpected host")
				}
			})
			initial := []causal.Endpoint{receiver, source}
			var other []causal.Endpoint
			if position == "discovery" {
				initial = []causal.Endpoint{empty}
				other = []causal.Endpoint{receiver, source}
			}
			rows, err := s.queryCoverage(context.Background(), initial, other, tree.Region{}, Request{Kind: "inventory"}, nil)
			if !errors.Is(err, unavailable) {
				t.Fatalf("mandatory receiver failure was hidden by source coverage: rows=%+v err=%v", rows, err)
			}
			if rows != nil {
				t.Fatal("failed read exposed partial rows")
			}
			if wire.count(receiver.Host) == 0 {
				t.Fatal("receiver was never attempted")
			}
		})
	}
}

func TestQueryCoverageJSONDistinguishesKnownEmptyFromUnknown(t *testing.T) {
	whole := tree.Region{}
	half := tree.Region{Mask: tree.ID{128}}
	for _, test := range []struct {
		name     string
		coverage []tree.Region
		wantErr  bool
	}{
		{"known_empty_whole", []tree.Region{whole}, false},
		{"unknown_half", []tree.Region{half}, true},
		{"unknown_whole", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			peer := coverageEndpoint("holder")
			s, _ := coverageService(func(string, Request) (Response, error) { return coveragePage(test.coverage), nil })
			rows, err := s.queryCoverage(context.Background(), []causal.Endpoint{peer}, nil, whole, Request{Kind: "inventory"}, nil)
			if test.wantErr {
				if !errors.Is(err, storage.ErrNotReady) || rows != nil {
					t.Fatalf("unknown scope became an empty success: %+v %v", rows, err)
				}
			} else if err != nil || len(rows) != 0 {
				t.Fatalf("known empty coverage did not survive JSON: %+v %v", rows, err)
			}
		})
	}
}

func TestQueryCoverageIgnoresOutOfScopeJSONReferral(t *testing.T) {
	left := tree.Region{Mask: tree.ID{128}}
	right := tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}
	source, unrelated := coverageEndpoint("source"), coverageEndpoint("unrelated")
	s, wire := coverageService(func(host string, r Request) (Response, error) {
		if host == unrelated.Host {
			return Response{}, errors.New("unrelated partition")
		}
		return coveragePage([]tree.Region{left}, storage.RegionReferral{Region: right, Endpoint: unrelated}), nil
	})
	rows, err := s.queryCoverage(context.Background(), []causal.Endpoint{source}, nil, left, Request{Kind: "inventory"}, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unrelated scope blocked complete empty read: %+v %v", rows, err)
	}
	if wire.count(unrelated.Host) != 0 {
		t.Fatal("queried referral outside requested region")
	}
}

func TestQueryCoverageJSONFollowsMandatoryLatestACK(t *testing.T) {
	source, receiver := coverageEndpoint("source"), coverageEndpoint("receiver")
	zone := tree.ZoneKey{Collection: "collection", Location: "@"}
	s, wire := coverageService(func(host string, r Request) (Response, error) {
		result := coveragePage([]tree.Region{{}})
		seq, id := uint64(1), "old"
		if host == source.Host {
			result.Page.Referrals = []storage.RegionReferral{{Region: tree.Region{}, Endpoint: receiver}}
		} else {
			seq, id = 2, "latest-ACK"
		}
		result.Inventory = []storage.Snapshot{{Zone: zone, Seq: seq, Records: []storage.Record{{ID: id}}}}
		return result, nil
	})
	rows, err := s.queryCoverage(context.Background(), []causal.Endpoint{source}, nil, tree.Region{}, Request{Kind: "inventory"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Seq != 2 || len(rows[0].Records) != 1 || rows[0].Records[0].ID != "latest-ACK" {
		t.Fatalf("lost latest ACK across JSON referral: %+v", rows)
	}
	if wire.count(receiver.Host) != 1 {
		t.Fatal("mandatory referral did not survive JSON")
	}
}
