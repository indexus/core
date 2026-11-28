package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// supply fills one requirement of a request, by the requirement's name.
var supply = map[string]func(*Request, causal.Endpoint){
	"Membership":    func(r *Request, _ causal.Endpoint) { r.Membership = &MembershipPacket{} },
	"Page":          func(r *Request, _ causal.Endpoint) { r.Page = &storage.InventoryPageRequest{} },
	"Cession":       func(r *Request, _ causal.Endpoint) { r.Cession = &causal.Cession{} },
	"Transfer":      func(r *Request, _ causal.Endpoint) { r.Transfer = &storage.CausalTransfer{} },
	"ZoneTransfers": func(r *Request, _ causal.Endpoint) { r.ZoneTransfers = []causal.ZoneTransfer{{}} },
	"Cold":          func(r *Request, _ causal.Endpoint) { r.Cold = &ColdProposal{} },
	"Retirement":    func(r *Request, _ causal.Endpoint) { r.Retirement = &causal.Member{} },
	"Owner":         func(r *Request, peer causal.Endpoint) { r.Owner = peer },
	"Record":        func(r *Request, _ causal.Endpoint) { r.Record = &storage.Record{} },
	"RequestID":     func(r *Request, _ causal.Endpoint) { r.RequestID = "request" },
	"RecordID":      func(r *Request, _ causal.Endpoint) { r.RecordID = "record" },
	"Seals or Transfer.Snapshots": func(r *Request, _ causal.Endpoint) {
		r.Seals = []storage.ZoneSeal{{}}
	},
}

// requestWith is a request of kind that carries every required field but
// the one at index without (none when without is negative).
func requestWith(t *testing.T, kind string, without int, peer causal.Endpoint) Request {
	t.Helper()
	r := Request{Kind: kind}
	for i, need := range required[kind] {
		fill := supply[need.name]
		if fill == nil {
			t.Fatalf("requirement %q has no test value", need.name)
		}
		if i != without {
			fill(&r, peer)
		}
	}
	return r
}

// diskState is what a refused request must leave as it was.
type diskState struct {
	Causal     storage.CausalState
	Directory  []storage.RegionReferral
	Rights     []causal.Right
	Cold       storage.ColdStatus
	Fallback   []tree.ZoneKey
	Membership causal.MembershipDelta
	Heard      int
}

func readDiskState(t *testing.T, s *Service) (d diskState) {
	t.Helper()
	var err error
	check := func() {
		if err != nil {
			t.Fatal(err)
		}
	}
	d.Causal, err = s.Store.CausalState()
	check()
	d.Directory, err = s.Store.ReadDirectory()
	check()
	d.Rights, err = s.Store.Rights(tree.Region{})
	check()
	d.Cold, err = s.Store.ColdStatus()
	check()
	d.Fallback, err = s.Store.FallbackZones()
	check()
	_, _, d.Membership, err = s.Store.Membership()
	check()
	s.ownerHeardMu.Lock()
	d.Heard = len(s.ownerHeard)
	s.ownerHeardMu.Unlock()
	return d
}

func TestRequestMissingRequiredFieldIsRefusedWithoutEffect(t *testing.T) {
	_, nodes := meshFixture(t, 2)
	s, peer := nodes[0], nodes[1].Self
	before := readDiskState(t, s)
	for _, kind := range slices.Sorted(maps.Keys(required)) {
		full := requestWith(t, kind, -1, peer)
		if err := full.validate(); err != nil {
			t.Errorf("%s with every required field: %v", kind, err)
		}
		for i, need := range required[kind] {
			r := requestWith(t, kind, i, peer)
			if err := r.validate(); !errors.Is(err, causal.ErrConflict) {
				t.Errorf("%s without %s: validate = %v, want causal.ErrConflict", kind, need.name, err)
			}
			if _, err := s.Handle(context.Background(), r); !errors.Is(err, causal.ErrConflict) {
				t.Errorf("%s without %s: Handle = %v, want causal.ErrConflict", kind, need.name, err)
			}
		}
		if kind != KindMembership {
			full.Membership = &MembershipPacket{}
			if _, err := s.Handle(context.Background(), full); !errors.Is(err, causal.ErrConflict) {
				t.Errorf("%s with a membership packet: Handle = %v, want causal.ErrConflict", kind, err)
			}
		}
	}
	if after := readDiskState(t, s); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused requests changed the disk:\nbefore %+v\nafter  %+v", before, after)
	}
}

// A lot travels by its seals or by its snapshots: stage-zone needs one of
// them, install-zone accepts either and neither.
func TestRequestAlternativesKeepTheirAcceptance(t *testing.T) {
	seals := []storage.ZoneSeal{{}}
	snapshots := &storage.CausalTransfer{Snapshots: []storage.Snapshot{{}}}
	moved := []causal.ZoneTransfer{{}}
	for _, c := range []struct {
		name string
		r    Request
		ok   bool
	}{
		{"stage-zone with seals", Request{Kind: "stage-zone", Seals: seals}, true},
		{"stage-zone with snapshots", Request{Kind: "stage-zone", Transfer: snapshots}, true},
		{"stage-zone with both", Request{Kind: "stage-zone", Seals: seals, Transfer: snapshots}, true},
		{"stage-zone with an empty transfer", Request{Kind: "stage-zone", Transfer: &storage.CausalTransfer{}}, false},
		{"stage-zone alone", Request{Kind: "stage-zone"}, false},
		{"install-zone alone", Request{Kind: "install-zone", ZoneTransfers: moved}, true},
		{"install-zone with seals", Request{Kind: "install-zone", ZoneTransfers: moved, Seals: seals}, true},
		{"install-zone with snapshots", Request{Kind: "install-zone", ZoneTransfers: moved, Transfer: snapshots}, true},
		{"install-zone with an empty transfer", Request{Kind: "install-zone", ZoneTransfers: moved, Transfer: &storage.CausalTransfer{}}, true},
		{"install-zone without zones", Request{Kind: "install-zone", Seals: seals, Transfer: snapshots}, false},
		{"unknown kind", Request{Kind: "no-such-kind"}, true},
	} {
		if err := c.r.validate(); (err == nil) != c.ok || err != nil && !errors.Is(err, causal.ErrConflict) {
			t.Errorf("%s: validate = %v, accepted want %v", c.name, err, c.ok)
		}
	}
}

// Every kind Handle dispatches has its line in required, and required names
// no other: a new kind cannot be served before its contract is decided.
func TestEveryDispatchedKindHasAContract(t *testing.T) {
	dispatched := dispatchedKinds(t)
	if len(dispatched) == 0 {
		t.Fatal("no kind found in Handle")
	}
	for _, kind := range dispatched {
		if _, ok := required[kind]; !ok {
			t.Errorf("Handle dispatches %q, which required does not list", kind)
		}
	}
	for kind := range required {
		if !slices.Contains(dispatched, kind) {
			t.Errorf("required lists %q, which Handle does not dispatch", kind)
		}
	}
}

// dispatchedKinds reads the cases of the switch on r.Kind in Service.Handle
// from the package source, resolving the Kind constants.
func dispatchedKinds(t *testing.T) (kinds []string) {
	t.Helper()
	files := token.NewFileSet()
	packages, err := parser.ParseDir(files, ".", func(info fs.FileInfo) bool { return !strings.HasSuffix(info.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	constants := map[string]string{}
	var handle *ast.FuncDecl
	for _, file := range packages["mesh"].Files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == "Handle" && d.Recv != nil {
					handle = d
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					v, ok := spec.(*ast.ValueSpec)
					if !ok || d.Tok != token.CONST {
						continue
					}
					for i, name := range v.Names {
						if i < len(v.Values) {
							if lit, ok := v.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
								constants[name.Name], _ = strconv.Unquote(lit.Value)
							}
						}
					}
				}
			}
		}
	}
	if handle == nil {
		t.Fatal("Service.Handle not found")
	}
	ast.Inspect(handle, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		if tag, ok := sw.Tag.(*ast.SelectorExpr); !ok || tag.Sel.Name != "Kind" {
			return true
		}
		for _, clause := range sw.Body.List {
			for _, e := range clause.(*ast.CaseClause).List {
				switch e := e.(type) {
				case *ast.BasicLit:
					kind, _ := strconv.Unquote(e.Value)
					kinds = append(kinds, kind)
				case *ast.Ident:
					kind, ok := constants[e.Name]
					if !ok {
						t.Fatalf("case %s of Handle is not a string constant of the package", e.Name)
					}
					kinds = append(kinds, kind)
				default:
					t.Fatalf("case of Handle is neither a literal nor a constant: %T", e)
				}
			}
		}
		return false
	})
	return kinds
}

// The JSON encoding lists fields in declaration order. Nodes of different
// versions share it: a field is added at will, never moved or retagged.
const requestLayout = "" +
	"Iterative,omitempty Directory,omitempty Locations,omitempty " +
	"Cold,omitempty Retirement,omitempty Kind Membership,omitempty " +
	"RoutingDigest,omitzero Page,omitempty Zone,omitzero Region,omitzero " +
	"Input,omitzero Target,omitzero Owner,omitzero TargetVNode,omitzero " +
	"Operation,omitzero Transfer,omitempty Cession,omitempty " +
	"ZoneTransfers,omitempty Seals,omitempty Mutations,omitempty " +
	"Batch,omitempty Record,omitempty RequestID,omitempty Visited,omitempty " +
	"Hops,omitempty Subdivide,omitempty Spawned,omitempty " +
	"Contributions,omitempty SummaryPage,omitempty Zones,omitempty " +
	"FallbackEntries,omitempty RecordID,omitempty Collection,omitempty " +
	"Description,omitempty Descriptions,omitempty Host:-"

const responseLayout = "ColdUsage,omitempty " +
	"SummaryStatus,omitempty Directory,omitempty Endpoint,omitzero " +
	"ColdPin,omitempty ColdDirty,omitempty Membership,omitempty " +
	"RoutingDigest,omitzero Page,omitempty Rights,omitempty " +
	"Progress,omitempty Cession,omitempty Mutation,omitzero " +
	"AppliedRegion,omitempty AppliedBy,omitzero Applied,omitempty " +
	"AppliedCount,omitempty FallbackEntry,omitempty Record,omitempty " +
	"Degraded,omitempty Inventory,omitempty Contributions,omitempty " +
	"SummaryMore,omitempty Previous,omitempty AppliedLocally:- " +
	"Durability,omitempty Description,omitempty"

func TestRequestAndResponseWireLayoutIsFixed(t *testing.T) {
	for _, c := range []struct {
		value any
		want  string
	}{{Request{}, requestLayout}, {Response{}, responseLayout}} {
		kind := reflect.TypeOf(c.value)
		var got []string
		for i := range kind.NumField() {
			f := kind.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("json")
			if tag != "" && !strings.HasPrefix(tag, ",") {
				tag = ":" + tag
			}
			got = append(got, f.Name+tag)
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s wire layout changed:\ngot  %s\nwant %s", kind.Name(), strings.Join(got, " "), c.want)
		}
	}
	for want, value := range map[string]any{
		`{"Kind":"write"}`: Request{Kind: "write", Host: "local", grainKiB: 4},
		`{}`:               Response{AppliedLocally: true},
		`{"ColdUsage":{"Owned":4,"Capacity":10}}`: Response{ColdUsage: &ColdUsage{Owned: 4, Capacity: 10}},
		`{"Iterative":true,"Locations":["l"],"Kind":"write","RequestID":"a","Hops":2,"Spawned":true,"RecordID":"b"}`: Request{Kind: "write", Iterative: true, Locations: []string{"l"}, RequestID: "a", Hops: 2, Spawned: true, RecordID: "b"},
		`{"Degraded":true,"SummaryMore":true,"Durability":"local"}`:                                                  Response{Durability: DurabilityLocal, SummaryMore: true, Degraded: true},
	} {
		if raw, err := json.Marshal(value); err != nil || string(raw) != want {
			t.Errorf("encoding changed:\ngot  %s (%v)\nwant %s", raw, err, want)
		}
	}
}
