package mesh

import (
	"context"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

const (
	KindMembership        = "membership"
	KindRights            = "rights"
	KindSummaryContribute = "summary-contribute"
	KindSummaryGet        = "summary-get"
)

// Request is the one message of the mesh: Kind says which operation it is and
// which of the other fields it carries. The table required lists what a kind
// cannot be served without; Handle checks it before any work.
//
// The declaration order is the byte order of the JSON encoding, which nodes
// of different versions share: add a field, never move, rename or retype
// one. By purpose, the fields are:
//
//	identity, routing   Kind, Target, Owner, TargetVNode, RoutingDigest, Host
//	write               Record, RequestID, Visited, Hops, Iterative
//	transfer            Operation, Input, Region, Zone, Cession, Transfer,
//	                    ZoneTransfers, Seals, Mutations, Batch, Subdivide,
//	                    Spawned, grainKiB
//	membership          Membership, Directory
//	summaries           Contributions, SummaryPage, Locations
//	inventory           Page, Zone
//	collections         Collection, Description, Descriptions
//	cold                Cold, Retirement
//	shadow              Owner, Zones, Transfer, Batch
//	fallback            Owner, Record, RequestID, RecordID, Zone,
//	                    FallbackEntries
type Request struct {
	// Iterative means the caller conducts fallback discovery. Targets still
	// follow durable routing hints but do not recursively scan membership.
	Iterative bool `json:",omitempty"`

	Directory          []storage.RegionReferral `json:",omitempty"`
	Locations          []string                 `json:",omitempty"`
	Cold               *ColdProposal            `json:",omitempty"`
	Retirement         *causal.Member           `json:",omitempty"`
	Kind               string
	Membership         *MembershipPacket             `json:",omitempty"`
	RoutingDigest      causal.Digest                 `json:",omitzero"`
	Page               *storage.InventoryPageRequest `json:",omitempty"`
	Zone               tree.ZoneKey                  `json:",omitzero"`
	Region             tree.Region                   `json:",omitzero"`
	Input              causal.Digest                 `json:",omitzero"`
	Target, Owner      causal.Endpoint               `json:",omitzero"`
	TargetVNode        tree.VNodeDescriptor          `json:",omitzero"`
	Operation          causal.TransferID             `json:",omitzero"`
	Transfer           *storage.CausalTransfer       `json:",omitempty"`
	Cession            *causal.Cession               `json:",omitempty"`
	ZoneTransfers      []causal.ZoneTransfer         `json:",omitempty"`
	Seals              []storage.ZoneSeal            `json:",omitempty"`
	Mutations          []storage.Mutation            `json:",omitempty"`
	Batch              []storage.ZoneMutations       `json:",omitempty"`
	Record             *storage.Record               `json:",omitempty"`
	RequestID          string                        `json:",omitempty"`
	Visited            []causal.Owner                `json:",omitempty"`
	Hops               int                           `json:",omitempty"` // observation only, not a protocol limit
	Subdivide, Spawned bool                          `json:",omitempty"`
	// grainKiB is what the grain of this handoff holds; local, never sent.
	grainKiB             int
	Contributions        []Contribution          `json:",omitempty"`
	SummaryPage          *SummaryPage            `json:",omitempty"`
	Zones                []tree.ZoneKey          `json:",omitempty"`
	FallbackEntries      []storage.FallbackEntry `json:",omitempty"`
	RecordID, Collection string                  `json:",omitempty"`
	Description          []byte                  `json:",omitempty"`
	Descriptions         [][]byte                `json:",omitempty"`
	Host                 string                  `json:"-"`
}

type Contribution struct {
	// Incomplete invalidates a formerly complete subtree; never fold its value.
	Incomplete bool `json:",omitempty"`
	Epoch      uint64
	Holder     causal.Endpoint
	Collection string
	Location   string
	Metrics    []float64
	Revision   uint64
	Generation uint64
	Count      int
	// Exact is the one location of every record below Location, or empty
	// when there are several (or from a peer that does not send it). The
	// parent names that child by it (docs/protocol/query.md).
	Exact string `json:",omitempty"`
	// Digest is the hex SHA-256 of the collection's description, when it has
	// one (DESC-4): the parent folds the value only with that description.
	Digest string `json:",omitempty"`
}

// SummaryPage is the cursor of a paged boundary poll. Keys sort by collection
// then location; the page holds the keys strictly after (Collection, Location).
type SummaryPage struct {
	Collection, Location string
	Limit                int
}

// SummaryStatus versions cache completeness in the sender's persistent summary
// incarnation and volatile message stream, never in a membership/global epoch.
type SummaryStatus struct {
	Holder      causal.Endpoint
	Epoch       uint64
	Revision    uint64
	Unavailable bool
}

// Response is the one answer of the mesh; a kind fills only its own fields.
// As for Request, the declaration order is the JSON byte order and is fixed.
// By purpose, the fields are:
//
//	routing        Endpoint, RoutingDigest, Rights, Directory
//	write          Mutation, Previous, Applied, AppliedBy, AppliedRegion,
//	               AppliedLocally, Durability
//	transfer       Cession, Progress
//	membership     Membership
//	summaries      SummaryStatus, Contributions, SummaryMore
//	inventory      Inventory, Page
//	collections    Description
//	cold           ColdPin, ColdDirty
//	shadow         Progress
//	fallback       FallbackEntry, AppliedCount, Record, Degraded
type Response struct {
	ColdUsage      *ColdUsage               `json:",omitempty"`
	SummaryStatus  *SummaryStatus           `json:",omitempty"`
	Directory      []storage.RegionReferral `json:",omitempty"`
	Endpoint       causal.Endpoint          `json:",omitzero"`
	ColdPin        *storage.ColdPin         `json:",omitempty"`
	ColdDirty      bool                     `json:",omitempty"`
	Membership     *MembershipReply         `json:",omitempty"`
	RoutingDigest  causal.Digest            `json:",omitzero"`
	Page           *storage.InventoryPage   `json:",omitempty"`
	Rights         []causal.Right           `json:",omitempty"`
	Progress       map[string]uint64        `json:",omitempty"`
	Cession        *causal.Cession          `json:",omitempty"`
	Mutation       storage.Mutation         `json:",omitzero"`
	AppliedRegion  *tree.Region             `json:",omitempty"`
	AppliedBy      causal.Endpoint          `json:",omitzero"`
	Applied        bool                     `json:",omitempty"`
	AppliedCount   int                      `json:",omitempty"`
	FallbackEntry  *storage.FallbackEntry   `json:",omitempty"`
	Record         *storage.Record          `json:",omitempty"`
	Degraded       bool                     `json:",omitempty"`
	Inventory      []storage.Snapshot       `json:",omitempty"`
	Contributions  []Contribution           `json:",omitempty"`
	SummaryMore    bool                     `json:",omitempty"`
	Previous       *storage.Record          `json:",omitempty"`
	AppliedLocally bool                     `json:"-"`
	Durability     string                   `json:",omitempty"`
	Description    []byte                   `json:",omitempty"`
}

// requirement is one thing a request of some kind must carry.
type requirement struct {
	name    string
	present func(*Request) bool
}

var (
	needMembership    = requirement{"Membership", func(r *Request) bool { return r.Membership != nil }}
	needPage          = requirement{"Page", func(r *Request) bool { return r.Page != nil }}
	needCession       = requirement{"Cession", func(r *Request) bool { return r.Cession != nil }}
	needTransfer      = requirement{"Transfer", func(r *Request) bool { return r.Transfer != nil }}
	needZoneTransfers = requirement{"ZoneTransfers", func(r *Request) bool { return len(r.ZoneTransfers) > 0 }}
	needCold          = requirement{"Cold", func(r *Request) bool { return r.Cold != nil }}
	needRetirement    = requirement{"Retirement", func(r *Request) bool { return r.Retirement != nil }}
	needOwner         = requirement{"Owner", func(r *Request) bool { return r.Owner.Valid() }}
	needRecord        = requirement{"Record", func(r *Request) bool { return r.Record != nil }}
	needRequestID     = requirement{"RequestID", func(r *Request) bool { return r.RequestID != "" }}
	needRecordID      = requirement{"RecordID", func(r *Request) bool { return r.RecordID != "" }}
	// A lot is staged either by its seals or by its snapshots.
	needSealsOrSnapshots = requirement{"Seals or Transfer.Snapshots", func(r *Request) bool {
		return len(r.Seals) > 0 || r.Transfer != nil && len(r.Transfer.Snapshots) > 0
	}}
)

// required is the contract of a request: for each kind Handle serves, what
// it cannot be served without. A request that lacks one is refused with
// causal.ErrConflict before any work. A kind with no entry after its name
// has no such field; its handler takes every field at its zero value, or
// refuses in its own way, as noted.
var required = map[string][]requirement{
	// membership and routing
	KindMembership: {needMembership},
	"directory":    nil,
	KindRights:     nil,
	"move-host":    nil,

	// data
	"write":               nil, // without a Record: storage.ErrNotReady, from its handler
	"inventory":           {needPage},
	KindInventoryPrefix:   {needPage},
	KindSummaryContribute: nil,
	KindSummaryGet:        nil,
	KindAdmitCollection:   nil, // one Collection, or else Descriptions
	KindGetCollection:     nil,

	// transfer, in protocol order
	"handoff":      nil,
	"admit":        {needCession},
	"cancel":       {needCession},
	"preload":      {needTransfer},
	"mirror":       nil, // a Batch, or else one Zone and its Mutations
	"stage":        {needTransfer},
	"stage-zone":   {needSealsOrSnapshots},
	"install":      {needCession},
	"install-zone": {needZoneTransfers}, // with Seals, with Transfer.Snapshots, or alone

	// cold storage
	KindColdUsage:   nil,
	KindColdPin:     {needCold},
	KindColdCheck:   {needCold},
	KindColdRelease: {needCold},
	KindColdWitness: {needCold, needRetirement},

	// shadows
	KindShadowSeqs:    {needOwner},
	KindShadowPreload: {needOwner}, // and a Transfer, refused by its handler once the owner is heard
	KindShadowMirror:  {needOwner},

	// fallback
	KindOwnerHeartbeat: {needOwner},
	KindFallbackWrite:  {needRecord, needRequestID},
	KindFallbackRepair: nil,
	KindDegradedRead:   {needOwner, needRecordID},
}

// validate refuses a request that lacks a field its kind requires, or that
// carries a membership packet under another kind. An unknown kind has no
// requirement here: Handle names it in its own error.
func (r *Request) validate() error {
	if r.Kind != KindMembership && r.Membership != nil {
		return causal.ErrConflict
	}
	for _, need := range required[r.Kind] {
		if !need.present(r) {
			return causal.ErrConflict
		}
	}
	return nil
}

type Transport interface {
	Exchange(context.Context, string, Request) (Response, error)
}
