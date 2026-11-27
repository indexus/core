package mesh

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// Collection descriptions (docs/protocol/collection-description.md) travel
// between disks by these requests. A description is self-certifying (its
// name is its hash) and immutable, so the catalog of a cluster is a
// grow-only set: every disk admits what it is sent, and a disk that holds
// zones of a collection holds its description (DESC-1):
//
//   - admission is broadcast to every disk of the routing view before it is
//     acknowledged (AdmitEverywhere);
//   - a disk handed zones (a handoff, a shadow copy) first receives every
//     description its donor holds (DESC-3, shipCatalog);
//   - a disk that meets an unknown digest asks the sender (FetchCollection).
const (
	KindAdmitCollection = "admit-collection"
	KindGetCollection   = "get-collection"
)

// ErrUnknownCollection: the asked disk holds no description of the name.
var ErrUnknownCollection = errors.New("mesh: no description held for this collection")

func (s *Service) handleCollection(request Request, out Response) (Response, error) {
	switch request.Kind {
	case KindAdmitCollection:
		if request.Collection == "" {
			return out, s.Store.AdmitDescriptions(request.Descriptions)
		}
		_, err := s.Store.AdmitCollection(request.Collection, request.Description)
		return out, err
	default: // KindGetCollection
		c, err := s.Store.CollectionOf(request.Collection)
		if err != nil {
			return out, err
		}
		if c == nil {
			return out, ErrUnknownCollection
		}
		out.Description = c.Bytes
		return out, nil
	}
}

// AdmitEverywhere admits a description on this disk, then on every other
// host of the routing view. It reports how many hosts hold it; an error
// means at least one host was not reached or refused it (a collision is
// never merged), and the caller retries: admission is idempotent.
func (s *Service) AdmitEverywhere(ctx context.Context, name string, ixc1 []byte) (int, error) {
	if _, err := s.Store.AdmitCollection(name, ixc1); err != nil {
		return 0, err
	}
	hosts, err := s.viewHosts()
	if err != nil {
		return 1, err
	}
	held, failed := 1, 0
	var first error
	for _, host := range hosts {
		_, err := s.Exchange(ctx, host, Request{Kind: KindAdmitCollection, Collection: name, Description: ixc1})
		if err != nil {
			failed++
			if first == nil {
				first = fmt.Errorf("%s: %w", host, err)
			}
			continue
		}
		held++
	}
	if failed > 0 {
		return held, fmt.Errorf("mesh: description held by %d of %d hosts: %w", held, held+failed, first)
	}
	return held, nil
}

// shipCatalog sends a host every description this disk holds, in one
// request, before any zone goes to it (DESC-3). The catalog is small and
// admission idempotent, so it is sent whole each time.
func (s *Service) shipCatalog(ctx context.Context, target causal.Endpoint) error {
	descriptions := s.Store.Descriptions()
	if len(descriptions) == 0 {
		return nil
	}
	_, err := s.endpointCall(ctx, target, Request{Kind: KindAdmitCollection, Descriptions: descriptions})
	return err
}

// FetchCollection asks a host for a description, verifies it and admits it.
func (s *Service) FetchCollection(ctx context.Context, host, name string) (*storage.Collection, error) {
	if c, err := s.Store.CollectionOf(name); err != nil || c != nil {
		return c, err
	}
	out, err := s.Exchange(ctx, host, Request{Kind: KindGetCollection, Collection: name})
	if err != nil {
		return nil, err
	}
	return s.Store.AdmitCollection(name, out.Description)
}

// viewHosts lists the other hosts that hold write authority somewhere.
func (s *Service) viewHosts() ([]string, error) {
	snapshot, err := s.Store.CausalRouting()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var hosts []string
	for _, member := range snapshot.Current().Active {
		endpoint, e := snapshot.Placement(member.Name)
		if e != nil {
			return nil, e
		}
		if h := endpoint.Host; h != "" && h != s.Self.Host && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	return hosts, nil
}
