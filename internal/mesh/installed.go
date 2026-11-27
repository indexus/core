package mesh

import (
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

const installedTrackLimit = 65536

func (s *Service) noteZonesInstalled(transfers []causal.ZoneTransfer, at time.Time) {
	if len(transfers) == 0 {
		return
	}
	s.installedMu.Lock()
	if s.installedAt == nil {
		s.installedAt = make(map[tree.Key]time.Time, len(transfers))
	}
	for _, tr := range transfers {
		if len(s.installedAt) >= installedTrackLimit {
			break
		}
		k := tr.Zone.Key()
		if _, exists := s.installedAt[k]; !exists {
			s.installedAt[k] = at
		}
	}
	s.installedCount.Store(int64(len(s.installedAt)))
	s.installedMu.Unlock()
}

func (s *Service) observeInstalledFirstWrite(record storage.Record) {
	if s.installedCount.Load() == 0 {
		return
	}
	zone, err := tree.NewZoneKey(record.Collection, record.Location)
	if err != nil {
		return
	}
	k := zone.Key()
	s.installedMu.Lock()
	at, ok := s.installedAt[k]
	if ok {
		delete(s.installedAt, k)
		s.installedCount.Store(int64(len(s.installedAt)))
	}
	s.installedMu.Unlock()
	if ok {
		metrics.Handoffs.InstallToFirstWrite.Observe(time.Since(at))
	}
}
