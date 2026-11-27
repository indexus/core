package modelcheck

import (
	"fmt"
	"testing"
	"time"
)

type summaryLocality uint8

const (
	sameVNode summaryLocality = iota
	sameMachine
	remoteMachine
)

func summaryWindow(locality summaryLocality) time.Duration {
	switch locality {
	case sameVNode:
		return 0
	case sameMachine:
		return 10 * time.Millisecond
	default:
		return time.Second
	}
}

func TestSummaryLocalityWindows(t *testing.T) {
	want := map[summaryLocality]time.Duration{
		sameVNode:     0,
		sameMachine:   10 * time.Millisecond,
		remoteMachine: time.Second,
	}
	for locality, expected := range want {
		if got := summaryWindow(locality); got != expected {
			t.Fatalf("locality %d: window=%s want %s", locality, got, expected)
		}
	}
}

// Design model, NOT the production summary protocol. A node has at most the
// 64 direct children of the Indexus alphabet. A changed child sends at once
// when its previous attempt is at least one second old; otherwise changes are
// coalesced until that edge's deadline. Relays apply the same rule immediately.
//
// Fixed membership, integer counts, perfect clocks and 10ms links: this checks
// scheduling, fan-in, duplicate replacement and retry traffic. It proves
// neither crash recovery nor authority changes nor monetary precision.
func TestDirectChildSummaryModel(t *testing.T) {
	const fanout, step, window, fallback = 64, 10, 1000, 3000
	type contribution struct {
		version uint64
		count   int64
	}
	type vertex struct {
		local, published int64
		version          uint64
		children         [fanout]contribution
		dirty            bool
	}
	type message struct {
		source int
		value  contribution
	}

	for _, size := range []int{64, 4096, 262144} {
		for _, dropFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("nodes-%d/drop-first-%v", size, dropFirst), func(t *testing.T) {
				nodes := make([]vertex, size)
				lastAttempt := make([]int, size)
				firstLost := make([]bool, size)
				for i := range lastAttempt {
					lastAttempt[i] = -window
				}
				var pending []message
				var committed, unseenMS int64
				rootCalls, totalCalls, convergedAt := 0, 0, -1

				for now := 0; now <= 12000; now += step {
					for _, msg := range pending {
						parent, slot := (msg.source-1)/fanout, (msg.source-1)%fanout
						old := &nodes[parent].children[slot]
						if msg.value.version > old.version {
							*old = msg.value
							nodes[parent].dirty = true
						}
					}
					pending = nil

					// One durable modification per node, spread through the first
					// second. The modification itself marks the direct zone dirty.
					for i := range nodes {
						if now == (i*37%100)*step {
							nodes[i].local++
							nodes[i].dirty = true
							committed++
						}
					}

					for i := len(nodes) - 1; i >= 0; i-- {
						n := &nodes[i]
						if now-lastAttempt[i] < window || (!n.dirty && now-lastAttempt[i] < fallback) {
							continue
						}
						if n.dirty {
							n.published = n.local
							for _, child := range n.children {
								n.published += child.count
							}
							n.version++
							n.dirty = false
						}
						lastAttempt[i] = now
						if i == 0 {
							continue
						}
						totalCalls++
						if (i-1)/fanout == 0 {
							rootCalls++
						}
						if dropFirst && !firstLost[i] {
							firstLost[i] = true
							continue
						}
						msg := message{i, contribution{n.version, n.published}}
						pending = append(pending, msg, msg) // duplicate delivery
					}

					if nodes[0].published > committed {
						t.Fatal("double-counted a duplicate")
					}
					unseenMS += (committed - nodes[0].published) * step
					if committed == int64(size) && nodes[0].published == committed {
						convergedAt = now
						break
					}
				}
				if convergedAt < 0 {
					t.Fatal("fallback did not repair dropped updates")
				}
				mean := float64(unseenMS) / float64(size)
				if !dropFirst && mean > 3000 {
					t.Fatalf("mean freshness exceeds target: %.1fms", mean)
				}
				rootBound := fanout * (convergedAt/window + 2)
				if rootCalls > rootBound {
					t.Fatalf("root traffic %d exceeds direct-child bound %d", rootCalls, rootBound)
				}
				t.Logf("nodes=%d drop_first=%v mean_lag_ms=%.1f all_visible_ms=%d root_messages=%d mesh_messages=%d", size, dropFirst, mean, convergedAt, rootCalls, totalCalls)
			})
		}
	}
}

// A continuously changing child must publish immediately on an idle edge, then
// at most once per window, while eventually delivering its newest absolute value.
func TestDirectChildCoalescesWriteRamp(t *testing.T) {
	const step, window, writesEvery, writesFor = 10, 1000, 10, 3500
	lastAttempt, pending, current, published, calls := -window, false, 0, 0, 0
	for now := 0; now <= writesFor+window; now += step {
		if now < writesFor && now%writesEvery == 0 {
			current++
			pending = true
		}
		if pending && now-lastAttempt >= window {
			published = current
			lastAttempt = now
			pending = false
			calls++
		}
	}
	writes := writesFor / writesEvery
	if published != writes {
		t.Fatalf("published=%d want latest=%d", published, writes)
	}
	if calls != 5 { // t=0, 1s, 2s, 3s, then the final coalesced value at 4s.
		t.Fatalf("calls=%d want 5 for %d writes", calls, writes)
	}
}
