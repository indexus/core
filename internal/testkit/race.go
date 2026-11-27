//go:build race

package testkit

// Race reports whether the race detector is on: it slows a run several
// times, so a test with a fixed time budget scales it (RaceScale).
const Race = true
