package mesh

import (
	"context"
	"net"
	"net/http/httptrace"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// The HTTP/3 adapter exposes the addresses of its QUIC connection the same
// way as HTTP/1 and HTTP/2 expose those of the TCP connection.
type copyTestConnection struct {
	net.Conn
	local, peer *net.TCPAddr
}

func (c copyTestConnection) LocalAddr() net.Addr  { return c.local }
func (c copyTestConnection) RemoteAddr() net.Addr { return c.peer }

func TestPacingChargesLocalWorkAndCreditsCompletedRemoteWait(t *testing.T) {
	lip := netip.MustParseAddr("10.0.0.11")
	const (
		receiver   = "10.0.0.22"
		localAlias = "10.0.0.12"
	)
	for _, c := range []struct {
		name, peer        string
		earlyReply, retry bool
		want              time.Duration
	}{
		// At 100% pressure, the existing policy assigns 10% to copying.
		// 30 ms of work requires 270 ms idle; 70 ms are already covered.
		{"remote", receiver, false, false, 200 * time.Millisecond},
		{"H3 wildcard bind", receiver, false, false, 200 * time.Millisecond},
		{"same machine second IP", localAlias, false, false, 900 * time.Millisecond},
		{"response before request fully written", receiver, true, false, 900 * time.Millisecond},
		{"transparent retry", receiver, false, true, 900 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := &CopyBudget{localIPs: map[netip.Addr]bool{lip: true, netip.MustParseAddr(localAlias): true}}
				b.pressure.Store(&copyPressure{used: 100, target: 50})
				ctx := b.traceWarmLot(t.Context(), false)
				trace := httptrace.ContextClientTrace(TraceCopyReply(ctx))
				conn := copyTestConnection{local: &net.TCPAddr{IP: net.ParseIP(lip.String()), Port: 1}, peer: &net.TCPAddr{IP: net.ParseIP(c.peer), Port: 2}}
				start := time.Now()
				if c.name == "H3 wildcard bind" {
					conn.local.IP = net.IPv6unspecified
				}
				trace.GotConn(httptrace.GotConnInfo{Conn: conn})
				time.Sleep(15 * time.Millisecond) // donor encodes and sends
				if c.earlyReply {
					trace.GotFirstResponseByte()
				}
				trace.WroteRequest(httptrace.WroteRequestInfo{})
				if c.retry {
					trace.GotConn(httptrace.GotConnInfo{Conn: conn})
					trace.WroteRequest(httptrace.WroteRequestInfo{})
				}
				time.Sleep(70 * time.Millisecond)
				trace.GotFirstResponseByte()
				trace.GotFirstResponseByte()      // duplicate event cannot grant more credit
				time.Sleep(15 * time.Millisecond) // donor decodes and commits
				began := time.Now()
				if err := b.pace(ctx, "test", time.Since(start), false); err != nil {
					t.Fatal(err)
				}
				if got := time.Since(began); got != c.want {
					t.Fatalf("paused %v, want %v", got, c.want)
				}
			})
		})
	}
}

func TestCopyReplyTraceSkipsUnmeteredAndUrgentLots(t *testing.T) {
	ctx := context.Background()
	b := &CopyBudget{localIPs: map[netip.Addr]bool{netip.MustParseAddr("10.0.0.1"): true}}
	if TraceCopyReply(b.traceWarmLot(ctx, false)) != ctx {
		t.Fatal("gauged an idle machine")
	}
	b.pressure.Store(&copyPressure{used: 100, target: 50})
	if TraceCopyReply(b.traceWarmLot(ctx, true)) != ctx {
		t.Fatal("gauged an urgent repair")
	}
}
