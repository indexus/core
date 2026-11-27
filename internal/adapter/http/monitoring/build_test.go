package monitoring

import (
	"encoding/json"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"

	indexus "github.com/indexus/go-indexus-core/indexus"
)

func TestBuildMetadata(t *testing.T) {
	for _, modified := range []string{"true", "false", "", "unknown"} {
		t.Run(modified, func(t *testing.T) {
			got := buildMetadata(&debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc123"},
				{Key: "vcs.time", Value: "2026-11-10T00:00:00Z"},
				{Key: "vcs.modified", Value: modified},
				{Key: "-ldflags", Value: "private build configuration"},
			}})
			if got.Revision != "abc123" || got.RevisionTime != "2026-11-10T00:00:00Z" || got.GoVersion == "" || got.Target == "" {
				t.Fatalf("incomplete metadata: %+v", got)
			}
			if modified == "true" || modified == "false" {
				if got.Modified == nil || *got.Modified != (modified == "true") {
					t.Fatalf("wrong dirty state: %+v", got)
				}
			} else if got.Modified != nil {
				t.Fatal("unknown dirty state presented as clean")
			}
			raw, _ := json.Marshal(got)
			if strings.Contains(string(raw), "private") {
				t.Fatal("build configuration leaked")
			}
		})
	}
	if got := buildMetadata(nil); got.Modified != nil || got.Revision != "" {
		t.Fatalf("unstamped build claims VCS provenance: %+v", got)
	}
}

type statusService struct{ Service }

func (statusService) MeasuredOwnershipStats() (int, int)     { return 2, 3 }
func (statusService) Registered() ([]indexus.Contact, error) { return nil, nil }
func (statusService) Name() string                           { return "test" }
func (statusService) Items() int                             { return 7 }
func (statusService) ID() []byte                             { return []byte{1} }
func (statusService) ClientReady() bool                      { return true }
func (statusService) AutoscaleSnapshot() map[string]any      { return nil }

func TestStatusIncludesCachedBuildProvenance(t *testing.T) {
	h := NewHttpHandler("", statusService{}, "test version")
	dirty := true
	h.build = buildInfo{Revision: "artifact-revision", Modified: &dirty, GoVersion: "go-test", Target: "test/arch"}
	w := httptest.NewRecorder()
	h.Status(w, httptest.NewRequest("GET", "/status", nil))
	var body struct {
		Version string    `json:"version"`
		Build   buildInfo `json:"build"`
		Items   int       `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || body.Version != "test version" || body.Items != 7 || body.Build.Revision != "artifact-revision" || body.Build.Modified == nil || !*body.Build.Modified {
		t.Fatalf("unexpected status: %s", w.Body.String())
	}
}
