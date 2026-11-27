"""Deployment wiring of the AWS harness: static, no network or AWS resources.

A host only has what host.sh.tftpl downloads. On 2026-11-26 run.sh uploaded
elastic/facts.sh and the host template did not fetch it, so a fresh cluster
would have fallen back to the slot-count placement without saying so.
"""
from pathlib import Path
import re
import subprocess
import unittest

HERE = Path(__file__).resolve().parent
HOST = (HERE / "host.sh.tftpl").read_text()
RUN = (HERE / "run.sh").read_text()
ELASTIC = HERE.parent / "elastic"


class DeploymentWiringTests(unittest.TestCase):
    def test_steps_replay_seed_tag_and_independent_passes(self):
        for setting in ("STEP_SEED", "STEP_TAG"):
            self.assertTrue(all(setting in line for line in RUN.splitlines() if "env | grep -E" in line))
        script = (HERE / "scenarios.sh").read_text()
        steps = re.search(r"^steps\(\) \{.*?^}", script, re.M | re.S).group()
        # Run the real plan with the cloud and the load replaced by recorders.
        harness = r'''cluster() { :; }
phase() {
  while (( $# )); do
    case "$1" in -seed|-tag|-id-prefix) printf '%s=%s ' "$1" "$2";; esac
    shift
  done
  echo
}
PROVISIONER=fake STEP_VNODES=16 STEP_SEED=009 STEP_TAG=replay STEP_PASSES=3 steps
'''
        lines = subprocess.check_output(["bash", "-uc", steps + "\n" + harness], text=True).splitlines()
        self.assertEqual([line.strip() for line in lines], [
            "-seed=9 -tag=replay -id-prefix=",
            "-seed=10 -tag=replay -id-prefix=pass2-",
            "-seed=11 -tag=replay -id-prefix=pass3-",
        ])

    def test_dynamic_slot_policy_reaches_host_loader_and_capacity(self):
        self.assertIn('SLOTS=0', RUN)
        self.assertIn('policy_args+=(--resource-slots)', RUN)
        self.assertIn('-slots ${slots} -resource-slots=${resource_slots}', HOST)
        self.assertIn('Environment=RESOURCE_SLOTS=${resource_slots}', HOST)
        self.assertIn('RESOURCE_SLOTS=${resource_slots}', (HERE/'loader.sh.tftpl').read_text())
        self.assertIn('sys.argv[1] != "true" or r.get("alive")', (HERE/'scenarios.sh').read_text())

    def test_steps_use_the_requested_initial_vnodes(self):
        scenario = (HERE / "scenarios.sh").read_text()
        steps = scenario.split("steps() {", 1)[1].split('\nif [[', 1)[0]
        self.assertIn('cluster "${STEP_VNODES:-${SIZES%% *}}"', steps)
        self.assertNotIn('cluster 4 ', steps)
        self.assertIn('export STEP_VNODES="${STEP_VNODES:-${SIZES%% *}}"', RUN)
        self.assertIn('largest=$STEP_VNODES', RUN)
        self.assertEqual(RUN.count('SAMPLE|STEP_VNODES|STEP_RATE|'), 2)

    def test_host_fetches_every_elastic_hook(self):
        whole = 's3 cp --recursive "s3://${bucket}/bin/elastic/" /opt/indexus/elastic/' in HOST
        for hook in sorted(ELASTIC.glob("*.sh")):
            with self.subTest(hook=hook.name):
                self.assertTrue(whole or f"elastic/{hook.name}" in HOST, f"{hook.name} is uploaded but never fetched")
        self.assertIn('cp "$ROOT"/ops/experiment/elastic/*.sh "$BIN/elastic/"', RUN)
        self.assertIn('cp "$ROOT/ops/experiment/elastic/boot_supply.py" "$BIN/elastic/"', RUN)
        self.assertTrue(whole or 'elastic/boot_supply.py' in HOST)

    def test_fleet_observer_is_deployed_independently_of_pinned_loader(self):
        self.assertIn('$HERE/../fleet.py', RUN)
        scenario=(HERE/'scenarios.sh').read_text()
        self.assertIn('/plan/fleet.py',scenario)
        self.assertIn('python3 $live machines "$dir/agents" "$dir"',scenario)
        self.assertIn('machines.jsonl',(HERE.parent/'evidence.py').read_text())

    def test_agent_is_built_from_the_version_under_test(self):
        # The load generator may be pinned (LOADER_ROOT); the machine agent may not.
        self.assertRegex(RUN, r'\(cd "\$ROOT" && [^\n]*-o "\$BIN/agent" \./cmd/experiment\)')
        self.assertIn("ExecStart=/opt/indexus/agent agent ", HOST)
        self.assertNotIn("/opt/indexus/experiment", HOST)
        self.assertRegex(HOST, r"for f in [^\n]*\bagent\b")

    def test_dynamic_audit_uses_the_tested_protocol_with_a_pinned_loader(self):
        loader = (HERE / 'loader.sh.tftpl').read_text()
        scenario = (HERE / 'scenarios.sh').read_text()
        self.assertIn('/bin/agent" /opt/indexus/auditor', loader)
        self.assertIn('export AUDITOR=/opt/indexus/auditor', loader)
        self.assertNotIn('$X audit', scenario)
        self.assertEqual(scenario.count('-monitors-file "$dir/monitors" -issuer'), 2)

    def test_every_template_variable_is_rendered(self):
        used = set(re.findall(r"(?<!\$)\$\{([a-z_0-9]+)\}", HOST))
        given = set(re.findall(r"\b([a-z_0-9]+)=", RUN))
        self.assertEqual(used - given, set(), "host.sh.tftpl variables run.sh never passes to render")

    def test_resource_signals_reach_the_spawned_agent(self):
        self.assertIn('resource_signals="${RESOURCE_SIGNALS:-true}"', RUN)
        # Go bool flags need =value or the following flags become positional.
        self.assertIn('-resource-signals=${resource_signals}', HOST)
        agent = (HERE.parents[2] / "cmd/experiment/agent.go").read_text()
        self.assertIn('"resource_signals": c.resourceSignals', agent)

    def test_cpu_profile_reaches_the_h3_node_and_the_agent(self):
        self.assertIn("-debug-base ${debug_base}", HOST)
        quic = re.search(r'\(cd "\$ROOT/quic" && [^\n]*', RUN).group(0)
        self.assertIn("node_tags", quic)
        diagnostic = (HERE.parents[2] / "quic/cmd/node/diagnostic.go").read_text()
        self.assertIn("//go:build diagnostic", diagnostic)


if __name__ == "__main__":
    unittest.main()
