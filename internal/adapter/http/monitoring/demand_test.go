package monitoring

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

type demandService struct{ statusService }

func (demandService) StorageUsage() (int64, int64, error) { return 0, 0, nil }
func (demandService) CommitStats() map[string]any         { return nil }
func (demandService) SummaryMetrics() map[string]any      { return nil }
func (demandService) WriteDemand() map[string]any {
	return map[string]any{"arrived": int64(12), "in_flight": int64(8), "oldest_wait_ms": int64(3000), "acknowledged": int64(4)}
}

// /metrics exports demand counted on arrival beside the existing counters.
func TestMetricsExportWriteDemand(t *testing.T) {
	h := NewHttpHandler("", demandService{}, "test")
	w := httptest.NewRecorder()
	h.Metrics(w, httptest.NewRequest("GET", "/metrics", nil))
	var body struct {
		Demand map[string]int64 `json:"demand"`
		Writes map[string]any   `json:"writes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Demand["arrived"] != 12 || body.Demand["in_flight"] != 8 || body.Demand["oldest_wait_ms"] != 3000 {
		t.Fatalf("demand not exported: %s", w.Body.String())
	}
	if _, kept := body.Writes["acknowledged"]; !kept {
		t.Fatal("an existing metric name was lost")
	}
}

func (demandService) ItemsAge() (time.Duration, bool) { return 4 * time.Second, true }

// A vnode shows 0 items until its first count: the age of the count tells
// that 0 from an empty vnode.
func TestMetricsExportTheAgeOfTheItemCount(t *testing.T) {
	h := NewHttpHandler("", demandService{}, "test")
	w := httptest.NewRecorder()
	h.Metrics(w, httptest.NewRequest("GET", "/metrics", nil))
	var body struct {
		Storage struct {
			Counted bool  `json:"items_counted"`
			AgeMS   int64 `json:"items_age_ms"`
		} `json:"storage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Storage.Counted || body.Storage.AgeMS != 4000 {
		t.Fatalf("item count age not exported: %s", w.Body.String())
	}
}
