package provisioner

import (
	"log/slog"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

// One scale decision per machine, not per vnode (HOST-RELIEF-1).
//
// A vnode asks with reason "host_pressure" when what fires is the CPU or the
// memory of its machine, not its own load. Every vnode of that machine sees
// the same pressure and asks too: answering each would start one receiver,
// and one more copy, per vnode. The first admitted request owns the relief
// of its machine; while that relief is in flight the others are answered as
// already handled, with the success shape of a target asked twice.

// reasonHostPressure is the reason of a request fired by the machine.
const reasonHostPressure = "host_pressure"

// reliefWait is how long a relief that found no destination yet stays with
// its owner between two of its requests. After reliefWait without renewal, another vnode of the machine may take over.
const reliefWait = placementMemory

// hostRelief is the host-pressure spawn in flight for one donor machine.
type hostRelief struct {
	owner  string    // the vnode whose request carries it
	asking bool      // that request is being served now
	placed bool      // its receiver was started
	at     time.Time // the last answer to the owner
	dest   string    // the receiver's machine, once decided
}

// active reports the relief still in flight: asked, waiting for a machine to
// boot, or its receiver still joining. A receiver counts as joining for
// placementMemory before its agent shows it, then for as long as the facts
// say its machine receives, at most targetPatience.
func (r *hostRelief) active(now time.Time, machines []autoscale.MachineFacts) bool {
	age := now.Sub(r.at)
	switch {
	case r.asking:
		return true
	case !r.placed:
		return age < reliefWait
	case age < placementMemory:
		return true
	}
	return age < targetPatience && receiving(machines, r.dest)
}

// receiving reports that the facts show a vnode still joining on machine.
func receiving(machines []autoscale.MachineFacts, machine string) bool {
	for _, m := range machines {
		if m.Machine == machine {
			return m.Receiving > 0
		}
	}
	return false
}

// pressureDonor is the machine of a host-pressure requester, with the facts
// it was read from. It is "" for any other reason, without a facts hook, or
// when no agent reports the requester: the request then keeps the behaviour
// of a request by vnode.
func (s *Server) pressureDonor(req scaleReq) (string, []autoscale.MachineFacts) {
	if req.Reason != reasonHostPressure || req.RequesterID == "" || !s.localMode() {
		return "", nil
	}
	dir := s.LocalDir
	if dir == "" {
		dir = "ops/local"
	}
	machines := s.machineFacts(dir)
	return autoscale.MachineOf(machines, req.RequesterID), machines
}

// coalescedLocked reports that donor's relief is already in flight and that
// req must start nothing. The owner of a relief still waiting for a machine
// is let through: its request is the one being retried.
func (s *Server) coalescedLocked(req scaleReq, donor string, machines []autoscale.MachineFacts, now time.Time) bool {
	if s.relief == nil {
		s.relief = map[string]*hostRelief{}
	}
	r := s.relief[donor]
	if r == nil {
		return false
	}
	if !r.active(now, machines) {
		delete(s.relief, donor)
		return false
	}
	if !r.placed && !r.asking && r.owner == req.RequesterID {
		return false
	}
	if s.placementDecided == nil {
		s.placementDecided, s.placementApplied = map[string]int{}, map[string]int{}
	}
	s.placementDecided["host_coalesced"]++
	transfers := -1 // not said
	if req.TransfersInFlight != nil {
		transfers = *req.TransfersInFlight
	}
	slog.Info("host pressure already handled", "machine", donor, "requester", req.RequesterID,
		"owner", r.owner, "receiver_machine", r.dest, "transfers_in_flight", transfers)
	return true
}
