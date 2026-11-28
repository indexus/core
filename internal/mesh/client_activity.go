package mesh

import (
	"context"
	"sync/atomic"
)

// ClientActivity counts data API operations other than single-record writes,
// which the write path already measures. Nested reads of a batch share its
// lifetime. Each disk accounts its own work when a client read crosses peers.
type ClientActivity struct {
	arrivals atomic.Int64
	active   atomic.Int64
}

type clientActivityKey struct{}

func (a *ClientActivity) Begin(ctx context.Context) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Value(clientActivityKey{}) == a {
		return ctx, func() {}
	}
	a.active.Add(1)
	a.arrivals.Add(1)
	return context.WithValue(ctx, clientActivityKey{}, a), func() { a.active.Add(-1) }
}

func (a *ClientActivity) Sample() (arrivals, active int64) {
	return a.arrivals.Swap(0), a.active.Load()
}

// Only these calls can forward a data operation. Catalog installation for a
// transfer, summary repair and audit scans use unmarked contexts.
func (r Request) dataCall() bool {
	switch r.Kind {
	case "inventory", KindInventoryPrefix, KindSummaryGet, KindDegradedRead, KindGetCollection, KindAdmitCollection:
		return true
	}
	return false
}

func (r Request) CarriesClientWork() bool { return r.ClientWork && r.dataCall() }
