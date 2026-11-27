package monitoring

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUDPCountersReadsTheUdpRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snmp")
	snmp := "Tcp: RtoAlgorithm ActiveOpens\nTcp: 1 7\n" +
		"Udp: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors\n" +
		"Udp: 120 1 9 130 8 2\n" +
		"UdpLite: InDatagrams NoPorts\nUdpLite: 0 0\n"
	if err := os.WriteFile(path, []byte(snmp), 0o600); err != nil {
		t.Fatal(err)
	}
	got := udpCounters(path)
	if got["RcvbufErrors"] != 8 || got["SndbufErrors"] != 2 || got["InErrors"] != 9 || got["InDatagrams"] != 120 {
		t.Fatalf("udpCounters = %v", got)
	}
	if udpCounters(filepath.Join(t.TempDir(), "missing")) != nil {
		t.Fatal("a missing file must give no counters")
	}
}
