package protocol

// MachineID names a physical host. It is not a vnode and must not be used as
// an XOR ownership identity.
type MachineID string

// ResourceCounters are the four buckets that share one hard cap C
// (docs/protocol/04-protocol-spec.md C1–C3).
type ResourceCounters struct {
	Owned            int64
	Residual         int64
	Staging          int64
	ReservedIncoming int64
}

func (r ResourceCounters) PhysicalLoad() int64 {
	return r.Owned + r.Residual + r.Staging + r.ReservedIncoming
}

// ResourceOffer is what a machine advertises for P2P placement (Gate 3).
// XOR ownership is never derived from an offer.
type ResourceOffer struct {
	MachineID        MachineID
	AvailableDisk    int64
	AvailableWrite   int64
	AvailableNetwork int64
	ActiveHandoffs   int
	Residual         int64
	Generation       uint64
}

// Machine is the physical host. Several vnodes may share it. Capacity is never
// an XOR-ownership exception: a full machine refuses work or MOVE/spawns; it
// does not rewrite owner().
type Machine struct {
	ID          MachineID
	Capacity    int64 // C
	WriteCap    int64 // H
	Network     int64
	WriteLoad   int64
	MirrorLoad  int64
	CopyNetwork int64
}

func NewMachine(id string, capacity int64) *Machine {
	return &Machine{ID: MachineID(id), Capacity: capacity}
}

// BestDiskOffer picks the roomiest disk that is not a residual donor.
func BestDiskOffer(offers []ResourceOffer) (ResourceOffer, bool) {
	var best ResourceOffer
	found := false
	for _, o := range offers {
		if o.Residual > 0 || o.AvailableDisk <= 0 {
			continue
		}
		if !found || o.AvailableDisk > best.AvailableDisk {
			best, found = o, true
		}
	}
	return best, found
}

// Usage is the four buckets that share one hard cap C (spec C1–C3).
type Usage = ResourceCounters

type Offer = ResourceOffer
