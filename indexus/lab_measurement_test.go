package indexus

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type labWriteObservation struct {
	ID                           int64
	DueUS, StartUS, FinishUS     int64
	LatencyUS, IntendedUS, RPCUS int64
	BackoffUS                    int64
	Joined, Ready, Open, Frozen  int64
	Attempts                     int
	Direct, Degraded, Success    bool
	Errors                       []string `json:",omitempty"`
}

// Write after the measured work, never through the journal lock on the ACK path.
func labSaveObservations(path string, rows []labWriteObservation) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	encoder := json.NewEncoder(w)
	for _, row := range rows {
		if err = encoder.Encode(row); err != nil {
			break
		}
	}
	return errors.Join(err, w.Flush(), f.Close())
}

func labReconcilePlacement(ctx context.Context, nodes []*mesh.Service) error {
	var lastError error
	for {
		complete := true
		for _, node := range nodes {
			if err := ctx.Err(); err != nil {
				return errors.Join(err, lastError)
			}
			routing, err := node.Store.CausalRouting()
			if err != nil {
				return err
			}
			cell, err := tree.Cell(routing.Current(), node.VNode.Name)
			if err != nil {
				return err
			}
			rights, err := node.Store.Rights(cell)
			if err != nil {
				return err
			}
			pieces := make([]tree.Region, 0, len(rights))
			for _, right := range rights {
				pieces = append(pieces, right.Region)
			}
			if tree.CoversRegions(cell, pieces) {
				continue
			}
			complete = false
			if err := node.Rebalance(ctx); err != nil {
				lastError = err
			}
		}
		if complete {
			return nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(ctx.Err(), lastError)
		case <-timer.C:
		}
	}
}

type labAckTransport struct{}

func (labAckTransport) Exchange(context.Context, string, mesh.Request) (mesh.Response, error) {
	return mesh.Response{}, nil
}

func TestLabFaultSwitchAlsoControlsLostInstallReplies(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		l := &link{base: labAckTransport{}, faults: enabled, log: &journal{enc: json.NewEncoder(io.Discard)}}
		for attempt := range 2 {
			_, err := l.Exchange(context.Background(), "receiver", mesh.Request{Kind: "install-zone"})
			wantLoss := enabled && attempt == 0
			if (err != nil) != wantLoss {
				t.Fatalf("faults=%v attempt=%d err=%v", enabled, attempt, err)
			}
		}
	}
}
