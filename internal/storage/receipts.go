package storage

import (
	"encoding/json"
)

// RequestReceipt retains replay identity, not a second copy of the old value.
// A matching retry supplies the value and reconstructs its original Mutation.
// These receipts are retained indefinitely; no session expiry is implied.
type RequestReceipt struct {
	Digest     [32]byte
	RequestID  string
	Seq        uint64
	Collection string
	Location   string
}

func requestReceipt(m Mutation, recordDigest [32]byte) RequestReceipt {
	return RequestReceipt{Digest: recordDigest, RequestID: m.RequestID, Seq: m.Seq, Collection: m.Record.Collection, Location: m.Record.Location}
}

// replayReceipt answers a retried RequestID from the node-wide receipt. It
// reports whether the request was already known; a retry with different
// content is refused without failing its batch group.
func replayReceipt(tx *Tx, w *writeRequest, out *writeOutcome) (bool, error) {
	raw := tx.value(key(operationsBucket, w.requestKey))
	if raw == nil {
		return false, nil
	}
	var old RequestReceipt
	if err := json.Unmarshal(raw, &old); err != nil {
		return true, err
	}
	if old.Digest != w.digest || old.RequestID != w.requestID || old.Collection != w.record.Collection || old.Location != w.record.Location {
		out.refuse = ErrConflict
		return true, nil
	}
	out.result = Mutation{RequestID: w.requestID, Seq: old.Seq, Record: w.record}
	return true, nil
}

// putRequestReceipts stores the receipt twice: in the zone, so it travels with
// a handoff, and in the node-wide ledger that answers retries.
func putRequestReceipts(tx *Tx, zone zoneTable, w *writeRequest, m Mutation, totals *writeTotals) error {
	operationRaw, err := json.Marshal(requestReceipt(m, w.digest))
	if err != nil {
		return err
	}
	used := &totals.usage.Owned
	if err := zone.put(tx, receiptPrefix, w.requestKey, operationRaw, used); err != nil {
		return err
	}
	return tx.putCounted(key(operationsBucket, w.requestKey), w.requestKey, operationRaw, used)
}
