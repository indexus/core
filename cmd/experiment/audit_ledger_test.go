package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuditLedgerRejectsIncompleteEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acked.jsonl")
	if _, _, err := readAuditLedger(path); err == nil {
		t.Fatal("missing ACK ledger accepted")
	}
	good := "{\"c\":\"C\",\"l\":\"@\",\"i\":\"w0\"}\n"
	for _, tail := range []string{"{", "null\n"} {
		if err := os.WriteFile(path, []byte(good+tail), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readAuditLedger(path); err == nil {
			t.Fatal("partial evidence accepted")
		}
	}
}
