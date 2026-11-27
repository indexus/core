// Package modelcheck searches for a legal schedule that breaks an invariant.
//
// ROLE
//
//	Independent bounded explorers. Not a second Indexus, not a benchmark.
//
// MODEL
//
//	causal_test.go: three nodes, divergent routing views, every delivery and
//	crash boundary of one causal transfer. zone_tree_test.go: a parent zone
//	and its children handed off level by level. summary_tree_test.go: the
//	direct-child summary design model.
//
// INVARIANTS
//
//	AUTH-1 single writer under divergent views, ZONE-2 and MIG-1 parent after
//	children. It does not measure time-to-relief.
//
// NON-GOALS
//
//	No realistic dataset. No throughput. No HTTP.
//
// VERIFICATION
//
//	go test explores each bounded space in full.
package modelcheck
