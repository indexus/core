package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/http/p2p"
	"github.com/indexus/go-indexus-core/internal/adapter/meshhttp"
	"github.com/indexus/go-indexus-core/internal/auth"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

func configureCluster(config Config, node *indexus.Node, server *p2p.Handler, verifier *auth.Verifier, cert *auth.NodeCert) (*clusterBoot, error) {
	if config.Storage == "" {
		return nil, fmt.Errorf("causal runtime requires durable storage")
	}
	scheme := "http"
	if config.TLSDir != "" {
		scheme = "https"
	}
	endpoint := scheme + "://" + net.JoinHostPort(config.Advertise[0], strconv.Itoa(config.P2pPort))
	boot := &clusterBoot{config: config, endpoint: endpoint, client: meshhttp.NewClient()}
	boot.bootstraps = splitValues(config.TopologyEndpoints)
	if config.MoveFrom != "" {
		boot.bootstraps = []string{config.MoveFrom}
	}
	if len(boot.bootstraps) == 0 {
		for _, peer := range config.Bootstraps {
			boot.bootstraps = append(boot.bootstraps, scheme+"://"+net.JoinHostPort(peer.IP(), strconv.Itoa(peer.Port())))
		}
	}
	for _, host := range boot.bootstraps {
		u, err := url.Parse(host)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("bootstrap endpoint must be an HTTP(S) URL: %q", host)
		}
	}
	if config.Engine != "" && config.Engine != "zonelog" {
		return nil, fmt.Errorf("storage engine %q: only zonelog is left", config.Engine)
	}
	store, err := storage.OpenWithOptions(config.Storage+".causal.db", config.Name, config.ClusterID, config.ClusterCapacity, storage.Options{
		BatchWrites: true,
		BatchSize:   256,
		BatchDelay:  2 * time.Millisecond,
		OmitHistory: config.OmitHistory,
		MemoryBytes: int(config.StorageMemory),
	})
	if err != nil {
		return nil, err
	}
	boot.store = store
	holder, err := store.Endpoint(endpoint)
	if err != nil {
		boot.Close()
		return nil, err
	}
	boot.mesh = &mesh.Service{Store: store, Self: holder, VNode: indexus.VNodeDescriptor{Name: node.Name(), ID: indexus.FromSlice(node.ID())}, Peers: boot.client, WriteRate: config.WriteRate, Spawned: config.Spawned}
	copies, err := mesh.NewCopyBudget(config.CopyBudgetDir, config.CopySlots)
	if err != nil {
		boot.Close()
		return nil, fmt.Errorf("copy budget: %w", err)
	}
	boot.mesh.SetCopyBudget(copies)
	if cert != nil {
		raw, err := json.Marshal(cert)
		if err != nil {
			boot.Close()
			return nil, err
		}
		encoded := base64.StdEncoding.EncodeToString(raw)
		boot.client.Authenticate = func(r *http.Request) { r.Header.Set("X-Indexus-Node-Cert", encoded) }
	}
	server.Ready = boot.ready.Load
	server.Cluster = &meshhttp.Handler{Mesh: func(ctx context.Context, request mesh.Request) (mesh.Response, error) {
		if !boot.ready.Load() {
			return mesh.Response{}, storage.ErrNotReady
		}
		return node.MeshData(ctx, request)
	}, Authorize: func(req *http.Request) error {
		if !config.RequireAuth {
			return nil
		}
		if verifier == nil {
			return fmt.Errorf("node verifier unavailable")
		}
		raw, err := base64.StdEncoding.DecodeString(req.Header.Get("X-Indexus-Node-Cert"))
		if err != nil {
			return err
		}
		var certificate auth.NodeCert
		if err = json.Unmarshal(raw, &certificate); err != nil {
			return err
		}
		return verifier.VerifyPeerCert(&certificate)
	}}
	return boot, nil
}

// startMesh opens the vnode, then joins: the whole start in one call.
func (b *clusterBoot) startMesh(ctx context.Context, node *indexus.Node) error {
	if err := b.openMesh(ctx, node); err != nil {
		return err
	}
	return b.joinMesh(ctx, node)
}

// openMesh initializes the store from genesis or a bootstrap peer and
// enables the vnode: afterwards it answers peers and can be monitored. It
// does not acquire the vnode's cell (joinMesh).
//
// Steps, in order:
//  1. check the store against the identity pinned on disk, when there is one;
//  2. initialize an empty store, or check the cluster of an initialized one;
//  3. check that this endpoint is where the membership places the vnode;
//  4. pin the identity on disk when it was not yet;
//  5. enable the vnode, its compaction and its replication.
func (b *clusterBoot) openMesh(ctx context.Context, node *indexus.Node) error {
	identity, err := b.store.Identity()
	if err != nil {
		return err
	}
	manifestPath := b.config.Storage + ".causal-identity.json"
	pinned, err := b.checkPinnedIdentity(manifestPath, identity)
	if err != nil {
		return err
	}
	if err := b.initCausalState(ctx); err != nil {
		return err
	}
	if err := b.checkPlacement(); err != nil {
		return err
	}
	if !pinned {
		if err := writeCausalIdentity(manifestPath, causalIdentity{Cluster: b.config.ClusterID, VNode: b.config.Name, Storage: identity}); err != nil {
			return err
		}
	}
	if err := b.enableVNode(node); err != nil {
		return err
	}
	b.ready.Store(true)
	return nil
}

// checkPinnedIdentity reports whether an identity is pinned on disk. A pinned
// identity that is not this cluster, vnode and store means the store is not
// the one this vnode wrote: the data is missing.
func (b *clusterBoot) checkPinnedIdentity(manifestPath, identity string) (pinned bool, err error) {
	raw, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var manifest causalIdentity
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false, err
	}
	if manifest.Cluster != b.config.ClusterID || manifest.VNode != b.config.Name || manifest.Storage != identity {
		return false, storage.ErrMissingData
	}
	return true, nil
}

// initCausalState leaves the store with a causal state of this cluster: an
// initialized store is only checked, an empty one is initialized from a
// bootstrap peer or, without any, as the genesis of a new cluster.
func (b *clusterBoot) initCausalState(ctx context.Context) error {
	state, readErr := b.store.CausalState()
	if readErr == nil {
		if state.Genesis.Cluster != b.config.ClusterID {
			return storage.ErrWrongCluster
		}
		return nil
	}
	// Only a completely uninitialized store may bootstrap. Corrupt metadata must
	// be distinguished from absence by the storage layer, never silently reset.
	initialized, err := b.store.CausalInitialized()
	if err != nil {
		return err
	}
	if initialized {
		return readErr
	}
	if len(b.bootstraps) > 0 {
		return b.initFromBootstrap(ctx)
	}
	if !b.config.CreateCluster {
		return fmt.Errorf("uninitialized store: run node init -config FILE or configure a bootstrap peer")
	}
	return b.store.InitCausal(b.mesh.VNode, causal.Genesis{Cluster: b.config.ClusterID, Holder: b.mesh.Self})
}

// initFromBootstrap initializes the store with the membership of a bootstrap
// peer; a moving vnode is initialized as the receiver of its move.
func (b *clusterBoot) initFromBootstrap(ctx context.Context) error {
	membership, err := b.fetchMembership(ctx)
	if err != nil {
		return err
	}
	initialize := b.store.InitCausal
	if b.config.MoveFrom != "" {
		initialize = b.store.InitCausalReceiver
	}
	if err := initialize(membership.Root, membership.Genesis); err != nil {
		return err
	}
	return b.store.MergeMembership(membership.Delta)
}

// fetchMembership asks the bootstrap peers in turn, every 100 ms, until one
// answers with a membership body or ctx ends. A round stops at the first
// membership answer, with or without a body.
func (b *clusterBoot) fetchMembership(ctx context.Context) (*mesh.MembershipEnvelope, error) {
	for {
		for _, host := range b.bootstraps {
			response, err := b.client.Exchange(ctx, host, mesh.Request{Kind: "membership", Membership: &mesh.MembershipPacket{}, Owner: b.mesh.Self})
			if err != nil || response.Membership == nil {
				continue
			}
			if body := response.Membership.Packet.Body; body != nil {
				return body, nil
			}
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// checkPlacement refuses to open a vnode that the membership places at
// another endpoint: its data is not here. A move first makes the store the
// receiver, and a vnode that is moving or not yet a member is not checked.
func (b *clusterBoot) checkPlacement() error {
	current, err := b.store.CausalState()
	if err != nil {
		return err
	}
	if b.config.MoveFrom != "" {
		if err := b.store.InitCausalReceiver(current.Root, current.Genesis); err != nil {
			return err
		}
	}
	replica, err := current.MembershipReplica()
	if err != nil {
		return err
	}
	if !replica.Current().HasName(b.mesh.VNode.Name) || b.config.MoveFrom != "" || current.Moving != nil {
		return nil
	}
	placement, err := replica.Endpoint(b.mesh.VNode.Name)
	if err != nil {
		return err
	}
	if placement != b.mesh.Self {
		return storage.ErrMissingData
	}
	return nil
}

// enableVNode attaches the mesh to the node, then the compaction and the
// replication the configuration asks for.
func (b *clusterBoot) enableVNode(node *indexus.Node) error {
	if err := node.EnableCausalCluster(b.mesh); err != nil {
		return err
	}
	if !b.config.DisableCompaction {
		if err := node.EnableCausalCompaction(b.config.Cold); err != nil {
			return err
		}
	}
	if m := b.config.ReplicationMachine; m != "" && m != "host" && m != "endpoint" {
		return fmt.Errorf("replication_machine %q: want host or endpoint", m)
	}
	switch b.config.Replication {
	case "":
	case "sync", "ack":
		if err := node.EnableReplication(b.config.ReplicationMachine == "endpoint", b.config.Replication == "ack"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("replication %q: want sync or ack", b.config.Replication)
	}
	return nil
}

// joinMesh acquires the vnode's cell from its donors, after openMesh.
func (b *clusterBoot) joinMesh(ctx context.Context, node *indexus.Node) error {
	cold, err := b.store.ColdStatus()
	if err != nil {
		return err
	}
	// Sealed/retired state is resumed by maintenance, never JOINed.
	if cold.Drain != nil {
		return nil
	}
	if b.config.MoveFrom != "" {
		return b.mesh.ReceiveMove(ctx, b.config.MoveFrom)
	}
	if err := node.JoinCausalCluster(ctx); err != nil {
		// The vnode is already enabled: maintenance and the retry below finish
		// acquiring its cell. Exiting here would free the slot and leave the
		// donors' half-done transfers behind (JOIN-2).
		return fmt.Errorf("%w: %w", errJoinUnfinished, err)
	}
	return nil
}

// errJoinUnfinished marks a join that ran out of startup time after the vnode
// was enabled; the process keeps running and keeps joining.
var errJoinUnfinished = errors.New("join unfinished at startup")

type causalIdentity struct{ Cluster, VNode, Storage string }

func writeCausalIdentity(path string, identity causalIdentity) error {
	raw, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// The identity must survive a process crash before any client ACK is served.
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
