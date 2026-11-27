// Public protocol facade. Addresses live in internal/tree, plans and ownership
// rules in internal/protocol; these aliases expose what the adapters use.
package indexus

import (
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Protocol types.
type HotInput = protocol.HotInput
type ID = tree.ID
type Key = tree.Key
type Region = tree.Region
type Machine = protocol.Machine
type TopologyPlan = protocol.TopologyPlan
type ZoneStats = protocol.ZoneStats
type ValidatedPlan = protocol.ValidatedPlan
type MachineState = protocol.MachineState
type Decision = protocol.Decision
type VNodeDescriptor = tree.VNodeDescriptor
type MachineID = protocol.MachineID

const (
	ReasonHotSplit  = protocol.ReasonHotSplit
	KindNone        = protocol.KindNone
	KindHotSplit    = protocol.KindHotSplit
	KindRegionalAdd = protocol.KindRegionalAdd
)

var BASE64 = tree.BASE64

var (
	MergeEncodings  = tree.MergeEncodings
	ChildCandidates = tree.ChildCandidates
	IsDirectChild   = tree.IsDirectChild
	EncodeName      = tree.EncodeName
	DecodeVnodeID   = tree.DecodeVnodeID
	CheckName       = tree.CheckName
	CheckLocation   = tree.CheckLocation
	Guard           = protocol.Guard
	PlanHotSplit    = protocol.PlanHotSplit
	FromSlice       = tree.FromSlice
	NewMachine      = protocol.NewMachine
	ValidatePlan    = protocol.ValidatePlan
	EvaluateMachine = protocol.EvaluateMachine
	ZoneID          = tree.ZoneID
	NewZoneKey      = tree.NewZoneKey
)
