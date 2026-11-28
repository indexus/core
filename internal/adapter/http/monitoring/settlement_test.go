package monitoring

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/indexus/go-indexus-core/internal/metrics"
)

// Embedding statusService leaves unrelated disk, summary and storage methods
// unimplemented: calling any of them from the census panics in this test.
type censusService struct {
	statusService
	err error
}

func (s censusService) ObserveFrozenTransfers() ([]string, error) { return []string{"frozen"}, s.err }
func (s censusService) Joining() bool                             { return true }

func TestSettlementReportsMovementWithoutFullMetrics(t *testing.T) {
	metrics.HandoffStarted("census-operation", "receiver")
	defer metrics.HandoffEnded("census-operation", true)
	for _, failure := range []error{nil, errors.New("read failed")} {
		h := NewHttpHandler("", censusService{err: failure}, "test")
		out := httptest.NewRecorder()
		h.Metrics(out, httptest.NewRequest("GET", "/metrics?view=settlement", nil))
		if failure != nil {
			if out.Code < 400 {
				t.Fatal("certified settlement after a failed store read")
			}
			continue
		}
		var doc struct {
			Name, ID string
			Joining  bool
			Frozen   []string `json:"frozen_transfers"`
			Handoffs struct{ Open struct{ Count int } }
		}
		if err := json.Unmarshal(out.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if out.Code != 200 || doc.Name != "test" || doc.ID != "01" || !doc.Joining || doc.Handoffs.Open.Count != 1 || len(doc.Frozen) != 1 || doc.Frozen[0] != "frozen" {
			t.Fatalf("wrong census: %s", out.Body.String())
		}
	}
}
