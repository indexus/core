package mesh

import (
	"context"
	"net"
	"net/http/httptrace"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Each lot issues its RPCs sequentially. Each transport attempt traces only
// the interval after its body was written and before the first response byte.
type copyReplies struct {
	local map[netip.Addr]bool
	wait  atomic.Int64
}
type copyRepliesKey struct{}

// The host budget already assumes vnodes on one machine share a network
// namespace. Include its alternate interface addresses in the local set.
func localCopyIPs() map[netip.Addr]bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	local := make(map[netip.Addr]bool, len(addrs))
	for _, addr := range addrs {
		if prefix, err := netip.ParsePrefix(addr.String()); err == nil {
			local[prefix.Addr().Unmap()] = true
		}
	}
	return local
}

func (b *CopyBudget) traceWarmLot(ctx context.Context, urgent bool) context.Context {
	if b == nil || urgent || len(b.localIPs) == 0 {
		return ctx
	}
	if p := b.pressure.Load(); p == nil || p.used <= p.target {
		return ctx
	}
	return context.WithValue(ctx, copyRepliesKey{}, &copyReplies{local: b.localIPs})
}

// TraceCopyReply is called by the HTTP transport once per exchange. Without
// both transport events, or with an unknown or local connection, use zero
// credit. A transparently retried exchange has ambiguous callbacks and also
// falls back to zero. The hooks can run on different goroutines.
func TraceCopyReply(ctx context.Context) context.Context {
	replies, _ := ctx.Value(copyRepliesKey{}).(*copyReplies)
	if replies == nil {
		return ctx
	}
	var mu sync.Mutex
	var connected, remote, answered bool
	var wrote time.Time
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			defer mu.Unlock()
			remote = false
			if connected || info.Conn == nil {
				return
			}
			connected = true
			local, e1 := netip.ParseAddrPort(info.Conn.LocalAddr().String())
			peer, e2 := netip.ParseAddrPort(info.Conn.RemoteAddr().String())
			ip := peer.Addr().Unmap()
			// An unconnected UDP socket (HTTP/3) can keep a wildcard
			// local address after the connection has selected its peer.
			ours := local.Addr().Unmap()
			remote = e1 == nil && e2 == nil && (ours.IsUnspecified() || replies.local[ours]) &&
				ip.IsGlobalUnicast() && !replies.local[ip]
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			mu.Lock()
			defer mu.Unlock()
			if info.Err != nil || !wrote.IsZero() {
				remote = false
			}
			wrote = time.Now()
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			defer mu.Unlock()
			if !answered && remote && !wrote.IsZero() {
				replies.wait.Add(int64(time.Since(wrote)))
			}
			answered = true
		},
	})
}
