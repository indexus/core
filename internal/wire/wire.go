package wire

// Peer wire budgets and the /sets projection are mirrored by sdk/api/wire.js.
// Shared fixtures verify both implementations without changing persisted data.
const (
	MaxReadLocations      = 1024
	MaxContentRecords     = 10000
	MaxContentParts       = 1024
	MaxContentBytes       = 64 << 20
	ContentLocationLength = 16
	SetsPrecision         = 32
	SetsPropertyCount     = 4
	SetsMetricScale       = 1_000_000
	HeaderDecodedBytes    = "X-Indexus-Decoded-Bytes"
	HeaderSHA256          = "X-Indexus-Sha256"
	HeaderStoredEncoding  = "X-Indexus-Stored-Encoding"
)
