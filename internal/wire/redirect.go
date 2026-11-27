package wire

// SetsRedirect names the peer a client should ask next for Location: the
// redirect section of an IXB2 body (docs/protocol/wire-block.md).
type SetsRedirect struct {
	Location string
	Name     string
	IP       string
	Port     int
}
