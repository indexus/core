# Sourced by the provisioner hooks: sets AGENTS, the agent URLs of the
# machines up now. Sources, first that applies: $INDEXUS_ELASTIC_DIR/agents
# (local, written by machine.sh), the objects under $AGENTS_S3.d/ (AWS: each
# host registers itself there once its agent answers), then $AGENTS as given.
E=${INDEXUS_ELASTIC_DIR:-}
if [[ -n "$E" && -f "$E/agents" ]]; then
  AGENTS=$(paste -sd, "$E/agents")
elif [[ -n "${AGENTS_S3:-}" ]]; then
  listed=$(aws s3 ls "$AGENTS_S3.d/" 2>/dev/null | awk '{print "http://" $NF ":22100"}' | paste -sd, -)
  AGENTS=${listed:-$(aws s3 cp "$AGENTS_S3" - 2>/dev/null || true)}
fi
