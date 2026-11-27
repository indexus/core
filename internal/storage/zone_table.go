package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"github.com/indexus/go-indexus-core/internal/tree"

	"github.com/indexus/go-indexus-core/internal/causal"
)

// The only storage layout: namespaced values in one bucket per zone.
var storageFormatKey = []byte("storage-format")

// Successor forwarding must survive receipt compaction from the first cession.
// Older layouts may have forgotten those routes; refuse them without migration.
var storageFormat = []byte("indexus-flat-4")

const (
	recordPrefix  byte = 1
	receiptPrefix byte = 2
)

// zoneTable is the path of a zone's bucket: live/<zone>, or
// staging/<operation>/<zone> for a staged copy. Its rows are the zone's
// sequence, its receipts (receiptPrefix) and, staged, its records
// (recordPrefix).
type zoneTable string

func liveZone(z tree.ZoneKey) zoneTable { return zoneTable(key(liveBucket, zoneBytes(z))) }

func (t zoneTable) row(name []byte) string { return string(t) + element(name) }

func (t zoneTable) entry(prefix byte, k []byte) string { return t.row(append([]byte{prefix}, k...)) }

func (t zoneTable) seq(tx *Tx) uint64 { return tx.number8(t.row(sequenceKey)) }

func (t zoneTable) get(tx *Tx, prefix byte, k []byte) []byte { return tx.value(t.entry(prefix, k)) }

// put writes the row k of table prefix and, with used, charges it there.
func (t zoneTable) put(tx *Tx, prefix byte, k, v []byte, used *int64) error {
	name := append([]byte{prefix}, k...)
	if used == nil {
		return tx.put(t.row(name), v)
	}
	return tx.putCounted(t.row(name), name, v, used)
}

func (t zoneTable) putJSON(tx *Tx, prefix byte, k []byte, v any) error {
	raw, err := json.Marshal(v)
	if err == nil {
		err = t.put(tx, prefix, k, raw, nil)
	}
	return err
}

// each visits the rows of table prefix after the key after ("": all), in
// order, with their keys.
func (t zoneTable) each(tx *Tx, prefix byte, after string, visit func(k, v []byte) error) error {
	from := ""
	if after != "" {
		from = t.entry(prefix, []byte(after)) + "\x00"
	}
	var failed error
	err := tx.each(string(t)+string([]byte{prefix}), from, func(path string, v []byte) bool {
		failed = visit(unescape(path[len(t):])[1:], v)
		return failed == nil
	})
	if err != nil {
		return err
	}
	return failed
}

// validatePermissionControl checks the sole current layout on reopen. CRC and
// transaction checks protect bytes; this checks local permission structure.
func validatePermissionControl(tx *Tx, node, cluster string) error {
	raw := tx.value(meta(causalKey))
	if raw == nil {
		return nil
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(sum[:], controlVersion(tx)) {
		return ErrConflict
	}
	state, e := readCausal(tx)
	if e != nil {
		return e
	}
	if state.Genesis.Cluster != cluster {
		return ErrWrongCluster
	}
	self := causal.Owner{VNode: node, Storage: storageID(tx)}
	if e = state.Permissions.Validate(self); e != nil {
		return e
	}
	if _, e = state.MembershipReplica(); e != nil {
		return e
	}
	tickets := map[uint64]bool{}
	for id, t := range state.Transfers {
		c := t.Cession
		if id != c.ID || !c.ValidOffer() || c.Cluster != cluster || t.Phase > causal.TransferCancelled {
			return ErrConflict
		}
		if c.Source.Owner == self {
			if id.Sequence > state.Permissions.Sequence {
				return ErrConflict
			}
			if t.Phase >= causal.TransferReady && t.Phase != causal.TransferCancelled && c.Ticket == 0 {
				return ErrConflict
			}
		} else if c.Target.Owner == self {
			if c.Ticket == 0 || c.Ticket > state.Permissions.Sequence || t.Phase == causal.TransferIssued || tickets[c.Ticket] {
				return ErrConflict
			}
			tickets[c.Ticket] = true
		} else {
			return ErrNotOwner
		}
	}
	return nil
}
