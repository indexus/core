#!/usr/bin/env bash
# Provisioner hook: nodes running across every agent.
source "$(dirname "$0")/agents.sh"
total=0
for agent in ${AGENTS//,/ }; do
  read -r n _ < <(curl -sf --max-time 2 "$agent/count") || continue
  total=$((total + n))
done
echo "$total"
