#!/usr/bin/env bash
# Provisioner hook: the resource facts of every machine up now, one JSON line
# per agent: {"agent": URL, "facts": {...}} (GET /facts, cmd/experiment agent).
# An agent that does not answer, or does not know /facts, has no line: the
# spawn hook then keeps its slot-count rule.
source "$(dirname "$0")/agents.sh"
for agent in ${AGENTS//,/ }; do
  facts=$(curl -sf --max-time 2 "$agent/facts") || continue
  printf '{"agent":"%s","facts":%s}\n' "$agent" "$facts"
done
