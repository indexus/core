# ops

Everything needed to run nodes outside the one-command local stack
(`scripts/local/explore.sh`).

| Folder | What | Entry point |
| --- | --- | --- |
| `config/` | Example node configuration (fields: [docs/production-configuration.md](../docs/production-configuration.md)) | `node.json` |
| `local/` | A local autoscaling mesh: issuer, bootstrap node, nodes spawned by the issuer ([local/README.md](local/README.md)) | `local/mesh_up.sh`, `local/mesh_down.sh` |
| `deploy/` | AWS lab: Linux binaries, AMI, Terraform ([docs/production-build.md](../docs/production-build.md)) | `deploy/scripts/deploy.sh` |

`deploy/scripts/build.sh` builds the production binaries alone; it is also
called by the dashboard's `ops/demo/deploy.sh`. `deploy/scripts/bake-ami.sh`
bakes the node AMI the Terraform launch template uses.
