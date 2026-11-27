package convergent

import "errors"

// Config holds the timers of section 5, in local milliseconds.
type Config struct {
	Beat    int64 // heartbeat period of the owner
	Suspect int64 // silence after which a replica enters fallback
	Ack     int64 // wait for a replica's copy before a local ACK
}

func DefaultConfig() Config { return Config{Beat: 500, Suspect: 3000, Ack: 1000} }

// ErrRedirect refuses fallback writes while the owner is still responsive.
var ErrRedirect = errors.New("convergent: not the owner and not in fallback")
