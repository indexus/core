package mesh

import (
	"sync"
	"time"
)

// writeCap simulates a vnode resource for scale experiments (CAP-2): at most
// rate writes per second are applied here, with one second of burst. A write
// over the cap is refused as overload, as a saturated disk would refuse it,
// so the vnode, not the shared test host, is the bottleneck. Zero disables.
type writeCap struct {
	mu     sync.Mutex
	tokens float64
	at     time.Time
}

func (c *writeCap) take(rate float64, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() {
		c.tokens = rate
	} else {
		c.tokens = min(rate, c.tokens+rate*now.Sub(c.at).Seconds())
	}
	c.at = now
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

func (c *writeCap) refund() {
	c.mu.Lock()
	c.tokens++
	c.mu.Unlock()
}
