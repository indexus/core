// Package meshhttp transports membership control and local handoff requests.
// It preserves typed refusals and control envelopes. Installed write permissions
// are checked by storage; data requests carry no global routing state.
package meshhttp
