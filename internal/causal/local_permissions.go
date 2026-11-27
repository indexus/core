package causal

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// Owner is a durable disk identity. Host addresses and endpoint epochs route
// messages; neither is a permission to write.
type Owner struct{ VNode, Storage string }

func (o Owner) Valid() bool { return o.VNode != "" && o.Storage != "" }

type Endpoint struct {
	Owner
	Host  string
	Epoch uint64
}

func (e Endpoint) Valid() bool { return e.Owner.Valid() && e.Host != "" }

// TransferID is allocated durably by its source, never reused for a new offer.
// It is not a global clock or an ancestry certificate.
type TransferID struct {
	Actor    string
	Sequence uint64
}

func (id TransferID) Valid() bool { return id.Actor != "" && id.Sequence != 0 }
func (id TransferID) MarshalText() ([]byte, error) {
	if !id.Valid() {
		return nil, ErrConflict
	}
	return []byte(id.Actor + ":" + strconv.FormatUint(id.Sequence, 10)), nil
}
func (id *TransferID) UnmarshalText(raw []byte) error {
	s := string(raw)
	colon := strings.LastIndexByte(s, ':')
	if colon <= 0 {
		return ErrConflict
	}
	n, err := strconv.ParseUint(s[colon+1:], 10, 64)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != s[colon+1:] {
		return ErrConflict
	}
	*id = TransferID{Actor: s[:colon], Sequence: n}
	return nil
}
func (id TransferID) String() string { return fmt.Sprintf("%s:%d", id.Actor, id.Sequence) }

// Right is installed authority. Its opaque identity is stable across warm
// copies and final data seals. No historical proof graph is needed to use it.
type Right struct {
	ID     Digest
	Region tree.Region
	Owner  Owner
}

func (r Right) Valid() bool { return r.ID != (Digest{}) && r.Region.Valid() && r.Owner.Valid() }

func rightID(cluster string, id TransferID, index uint32) Digest {
	return Hash(struct {
		Domain, Cluster string
		Operation       TransferID
		Index           uint32
	}{"indexus-installed-right", cluster, id, index})
}

// Cession grants exactly one piece to a peer. Its local complements are
// computed from Input.Region.Subtract(Region); they are not sent as history.
type Cession struct {
	Cluster    string
	ID         TransferID
	Input      Right
	Source     Endpoint
	Target     Endpoint
	Region     tree.Region
	Ticket     uint64
	Barrier    uint64
	DataDigest Digest
}

func (c Cession) ValidOffer() bool {
	inside, intersects := c.Input.Region.Intersect(c.Region)
	return c.Cluster != "" && c.ID.Valid() && c.Input.Valid() && c.Source.Valid() && c.Target.Valid() &&
		c.Input.Owner == c.Source.Owner && c.ID.Actor == c.Source.Storage && c.Source.Owner != c.Target.Owner &&
		c.Region.Valid() && intersects && inside == c.Region
}
func (c Cession) Right() Right {
	return Right{ID: rightID(c.Cluster, c.ID, 0), Region: c.Region, Owner: c.Target.Owner}
}
func (c Cession) Binding() Digest {
	c.Ticket, c.Barrier, c.DataDigest = 0, 0, Digest{}
	return Hash(c)
}
func (c Cession) Digest() Digest { return Hash(c) }

type TransferPhase uint8

const (
	TransferReserved  TransferPhase = iota // admission allocated, not confirmed
	TransferReady                          // first preload confirmed durably
	TransferMoving                         // first point fence/install; not cancellable
	TransferSealed                         // regional data certified/frozen
	TransferIssued                         // source consumed its native input
	TransferInstalled                      // native install, or source's durable ACK
	TransferCancelled                      // source abort persisted; receiver cleanup still pending
)

// PermissionTransfer is the metadata half of the transfer state machine.
// Storage embeds it with its staged bytes and per-zone progress, not a second
// copy of these permission decisions.
type PermissionTransfer struct {
	Cession Cession
	Phase   TransferPhase
}

type TicketState uint8

const (
	TicketOpen TicketState = iota
	TicketClosed
)

// LocalPermissions retains only installed rights and one monotone local
// sequence. Actual open/collecting transfers live in the storage operation map.
// Source operation IDs, receiver tickets and local coalescing share Sequence.
type LocalPermissions struct {
	Sequence      uint64
	EndpointEpoch uint64
	Native        []Right `json:",omitempty"`
}

func (p LocalPermissions) Clone() LocalPermissions {
	p.Native = slices.Clone(p.Native)
	return p
}
func (p LocalPermissions) Find(id Digest) (Right, bool) {
	for _, right := range p.Native {
		if right.ID == id {
			return right, true
		}
	}
	return Right{}, false
}
func (p LocalPermissions) At(id tree.ID) (Right, bool) {
	for _, right := range p.Native {
		if right.Region.Contains(id) {
			return right, true
		}
	}
	return Right{}, false
}
func (p LocalPermissions) advance() (LocalPermissions, uint64, error) {
	if p.Sequence == math.MaxUint64 {
		return p, 0, ErrConflict
	}
	p.Sequence++
	return p, p.Sequence, nil
}

func (p LocalPermissions) Begin(cluster string, inputID Digest, region tree.Region, source, target Endpoint) (LocalPermissions, PermissionTransfer, error) {
	input, exists := p.Find(inputID)
	if !exists || input.Owner != source.Owner {
		return p, PermissionTransfer{}, ErrConflict
	}
	next, sequence, err := p.advance()
	if err != nil {
		return p, PermissionTransfer{}, err
	}
	c := Cession{Cluster: cluster, ID: TransferID{Actor: source.Storage, Sequence: sequence},
		Input: input, Source: source, Target: target, Region: region}
	if !c.ValidOffer() {
		return p, PermissionTransfer{}, ErrConflict
	}
	return next, PermissionTransfer{Cession: c, Phase: TransferReserved}, nil
}

// Partition divides an installed local right before admission or copying.
// Every output stays on the same disk, so writes remain available throughout.
// A transfer can then pin only its requested piece, leaving the complements
// available to independent joins. Storage must reject an already pinned input.
func (p LocalPermissions) Partition(cluster string, inputID Digest, region tree.Region, self Owner) (LocalPermissions, Right, error) {
	input, found := p.Find(inputID)
	part, overlap := input.Region.Intersect(region)
	if cluster == "" || !found || !input.Valid() || input.Owner != self || !region.Valid() || !overlap || part != region {
		return p, Right{}, ErrConflict
	}
	if region == input.Region {
		return p, input, nil
	}
	next, sequence, err := p.advance()
	if err != nil {
		return p, Right{}, err
	}
	next = next.Clone()
	next.Native = slices.DeleteFunc(next.Native, func(r Right) bool { return r.ID == input.ID })
	id := TransferID{Actor: self.Storage, Sequence: sequence}
	selected := Right{ID: rightID(cluster, id, 0), Region: region, Owner: self}
	next.Native = append(next.Native, selected)
	for i, remainder := range input.Region.Subtract(region) {
		next.Native = append(next.Native, Right{ID: rightID(cluster, id, uint32(i+1)), Region: remainder, Owner: self})
	}
	return next, selected, nil
}

// Inspect validates the exact ticket before any staged bytes can be mounted.
// A closed ticket is an idempotent ACK, not a new reservation. Confirmed slots
// must not be forgotten until installation or pre-fence source cancellation.
func (p LocalPermissions) Inspect(c Cession, current *PermissionTransfer, self Owner) (TicketState, error) {
	if !c.ValidOffer() || c.Target.Owner != self || c.Ticket == 0 || c.Ticket > p.Sequence {
		return TicketClosed, ErrConflict
	}
	if current == nil {
		return TicketClosed, nil
	}
	if current.Cession.ID != c.ID || current.Cession.Binding() != c.Binding() {
		return TicketClosed, ErrConflict
	}
	if current.Cession.Ticket != c.Ticket {
		if c.Ticket < current.Cession.Ticket {
			return TicketClosed, nil // old packet, including a replayed old Admit
		}
		return TicketClosed, ErrConflict
	}
	if current.Phase == TransferCancelled {
		return TicketClosed, nil // only cleanup remains; never mount data
	}
	if current.Phase > TransferInstalled {
		return TicketClosed, ErrConflict
	}
	if current.Phase == TransferInstalled {
		return TicketClosed, nil
	}
	return TicketOpen, nil
}

// Reserve creates header-only tentative state. Repeated admissions reuse the
// current slot. A delayed zero-ticket request after collection may create a new
// tentative slot; old messages still carry their OLD ticket and cannot use it.
func (p LocalPermissions) Reserve(c Cession, current *PermissionTransfer, self Owner) (LocalPermissions, PermissionTransfer, TicketState, error) {
	if !c.ValidOffer() || c.Target.Owner != self {
		return p, PermissionTransfer{}, TicketClosed, ErrConflict
	}
	if c.Ticket != 0 {
		state, err := p.Inspect(c, current, self)
		if current != nil {
			return p, *current, state, err
		}
		return p, PermissionTransfer{}, state, err
	}
	if current != nil {
		if current.Cession.Binding() != c.Binding() || current.Cession.ID != c.ID {
			return p, PermissionTransfer{}, TicketClosed, ErrConflict
		}
		if current.Phase == TransferInstalled || current.Phase == TransferCancelled {
			return p, *current, TicketClosed, nil
		}
		return p, *current, TicketOpen, nil
	}
	next, sequence, err := p.advance()
	if err != nil {
		return p, PermissionTransfer{}, TicketClosed, err
	}
	c.Ticket = sequence
	return next, PermissionTransfer{Cession: c, Phase: TransferReserved}, TicketOpen, nil
}

func BindTicket(op PermissionTransfer, ticket uint64) (PermissionTransfer, error) {
	if ticket == 0 || op.Phase > TransferInstalled || op.Cession.Ticket != 0 && op.Cession.Ticket != ticket || op.Phase != TransferReserved && op.Cession.Ticket == 0 {
		return op, ErrConflict
	}
	op.Cession.Ticket = ticket
	return op, nil
}

// Confirm is committed with the first preload on the receiver, and with its
// successful ACK on the source. Source fencing is forbidden before this state.
func Confirm(op PermissionTransfer, c Cession) (PermissionTransfer, error) {
	if !c.ValidOffer() || op.Phase > TransferInstalled || c.Ticket == 0 || op.Cession.Ticket != c.Ticket || op.Cession.Binding() != c.Binding() || op.Phase == TransferInstalled {
		return op, ErrConflict
	}
	if op.Phase == TransferReserved {
		op.Phase = TransferReady
	}
	return op, nil
}

func (p LocalPermissions) Emit(op PermissionTransfer, self Owner) (LocalPermissions, PermissionTransfer, error) {
	c := op.Cession
	if !c.ValidOffer() || c.Source.Owner != self || c.ID.Sequence > p.Sequence || c.Ticket == 0 || c.DataDigest == (Digest{}) {
		return p, op, ErrConflict
	}
	if op.Phase > TransferInstalled {
		return p, op, ErrConflict
	}
	if op.Phase >= TransferIssued {
		return p, op, nil
	}
	input, exists := p.Find(c.Input.ID)
	if !exists || input != c.Input || op.Phase != TransferSealed {
		return p, op, ErrConflict
	}
	next := p.Clone()
	next.Native = slices.DeleteFunc(next.Native, func(r Right) bool { return r.ID == input.ID })
	for i, region := range input.Region.Subtract(c.Region) {
		next.Native = append(next.Native, Right{ID: rightID(c.Cluster, c.ID, uint32(i+1)), Region: region, Owner: self})
	}
	op.Phase = TransferIssued
	return next, op, nil
}

func (p LocalPermissions) Install(c Cession, current *PermissionTransfer, self Owner) (LocalPermissions, PermissionTransfer, TicketState, error) {
	state, err := p.Inspect(c, current, self)
	if err != nil || state == TicketClosed {
		return p, PermissionTransfer{}, state, err
	}
	if current.Phase != TransferSealed || current.Cession.Digest() != c.Digest() {
		return p, *current, TicketOpen, ErrConflict
	}
	in := c.Right()
	for _, right := range p.Native {
		if _, overlaps := right.Region.Intersect(in.Region); overlaps || right.ID == in.ID {
			return p, *current, TicketOpen, ErrConflict
		}
	}
	next := p.Clone()
	next.Native = append(next.Native, in)
	next.EndpointEpoch = max(next.EndpointEpoch, c.Target.Epoch)
	op := *current
	op.Phase = TransferInstalled
	return next, op, TicketClosed, nil
}

// Validate checks the durable local whitelist, without consulting membership.
func (p LocalPermissions) Validate(self Owner) error {
	if !self.Valid() {
		return ErrConflict
	}
	for i, r := range p.Native {
		if !r.Valid() || r.Owner != self {
			return ErrConflict
		}
		for _, other := range p.Native[:i] {
			if _, overlaps := r.Region.Intersect(other.Region); overlaps || r.ID == other.ID {
				return ErrConflict
			}
		}
	}
	return nil
}

// Coalesce consumes exactly two sibling native rights. Storage must reject
// rights pinned by an ongoing source transfer before calling this pure helper.
// A fresh durable sequence prevents replay from recreating either old identity.
// The input frontier must already be valid. Replacing two disjoint siblings
// with their exact union preserves its disjointness without a quadratic scan.
func (p LocalPermissions) Coalesce(cluster string, a, b Digest, self Owner) (LocalPermissions, Right, error) {
	x, ok := p.Find(a)
	y, found := p.Find(b)
	if cluster == "" || !ok || !found || !x.Valid() || !y.Valid() || a == b || x.Owner != self || y.Owner != self || x.Region.Mask != y.Region.Mask {
		return p, Right{}, ErrConflict
	}
	parent := x.Region
	differences := 0
	for i := range parent.Mask {
		bits := x.Region.Value[i] ^ y.Region.Value[i]
		for bit := byte(128); bit != 0; bit >>= 1 {
			if bits&bit != 0 {
				differences++
				parent.Mask[i] &^= bit
				parent.Value[i] &^= bit
			}
		}
	}
	if differences != 1 {
		return p, Right{}, ErrConflict
	}
	next, sequence, err := p.advance()
	if err != nil {
		return p, Right{}, err
	}
	merged := Right{ID: rightID(cluster, TransferID{Actor: self.Storage, Sequence: sequence}, 0), Region: parent, Owner: self}
	next = next.Clone()
	next.Native = slices.DeleteFunc(next.Native, func(r Right) bool { return r.ID == a || r.ID == b })
	next.Native = append(next.Native, merged)
	return next, merged, nil
}

// StartMoving must be persisted with the first fence (source) or installed
// point (receiver). Only Reserved and Ready operations can still be cancelled.
func StartMoving(op PermissionTransfer) (PermissionTransfer, error) {
	if op.Phase < TransferReady || op.Phase > TransferInstalled {
		return op, ErrConflict
	}
	if op.Phase == TransferReady {
		op.Phase = TransferMoving
	}
	return op, nil
}
func CanCancel(op PermissionTransfer) bool { return op.Phase <= TransferReady }

// Seal binds the certified final data to an already confirmed operation.
// Storage persists it with the regional fence/certified staging transaction.
func Seal(op PermissionTransfer, c Cession) (PermissionTransfer, error) {
	if !c.ValidOffer() || c.DataDigest == (Digest{}) || c.Ticket == 0 || op.Cession.Ticket != c.Ticket || op.Cession.Binding() != c.Binding() || op.Phase < TransferReady || op.Phase > TransferSealed {
		return op, ErrConflict
	}
	if op.Phase == TransferSealed && op.Cession.Digest() != c.Digest() {
		return op, ErrConflict
	}
	op.Cession, op.Phase = c, TransferSealed
	return op, nil
}
