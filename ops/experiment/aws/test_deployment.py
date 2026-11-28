"""Deployment wiring of the AWS harness: static, no network or AWS resources.

A host only has what host.sh.tftpl downloads. On 2026-11-26 run.sh uploaded
elastic/facts.sh and the host template did not fetch it, so a fresh cluster
would have fallen back to the slot-count placement without saying so.
"""
from pathlib import Path
import json
import re
import subprocess
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
HOST = (HERE / "host.sh.tftpl").read_text()
RUN = (HERE / "run.sh").read_text()
ELASTIC = HERE.parent / "elastic"


class DeploymentWiringTests(unittest.TestCase):
    def test_steps_replay_seed_tag_and_independent_passes(self):
        for setting in ("STEP_SEED", "STEP_TAG", "STEP_FOUNDERS", "AUDIT_SECONDS"):
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

    def test_founder_manifest_rejected_before_reset_or_provisioning(self):
        script = HERE / "scenarios.sh"
        a, b = "0" * 16, "_" * 16
        for names in [a, f"{a},{a}", f"{a},{b},", f"{a},short", f"{a},{'@' * 16}"]:
            with self.subTest(names=names):
                r = subprocess.run(["bash", str(script), "--check-founders", "2", names], capture_output=True)
                self.assertNotEqual(r.returncode, 0)
        self.assertEqual(subprocess.run(["bash", str(script), "--check-founders", "2", f"{a},{b}"]).returncode, 0)
        self.assertLess(RUN.index("--check-founders"), RUN.index('aws --region'))

    def test_founders_apply_to_the_correct_seed_spawn(self):
        scenario = (HERE / "scenarios.sh").read_text()
        funcs = "\n".join(re.search(r"^" + name + r"\(\) \{.*?^}", scenario, re.M | re.S).group()
                          for name in ("check_founders", "cluster", "build"))
        mock = r'''OUT=$1; AG=(a b); MODE=steps; RESOURCE_SLOTS=true
curl() { :; }
sleep() { :; }
endpoints() { :; }
seed_spawn() { echo "$2" >> "$OUT/requests"; echo "local-fake-slot"; }
founder_endpoint() { echo "http://peer|http://monitor"; }
balanced() { :; }
cluster 4 "$2" "$3"
'''
        names = [c * 16 for c in "0AZ_"]
        with tempfile.TemporaryDirectory() as tmp:
            cmd = ["bash", "-uc", funcs + "\n" + mock, "_", tmp]
            subprocess.run(cmd + ["compaction=false", ",".join(names)], check=True)
            requests = (Path(tmp) / "requests").read_text().splitlines()
            self.assertEqual([r.split("/")[0] for r in requests], ["a", "b", "a", "b"])
            self.assertEqual([r.split("founder=")[1] for r in requests], names)
            bad = subprocess.run(cmd + ["prefer_near=" + names[0], ",".join(names)], capture_output=True)
            self.assertNotEqual(bad.returncode, 0)
            self.assertEqual((Path(tmp) / "requests").read_text().splitlines(), requests)

    def test_initial_xor_cells_use_branch_depth_and_unique_names(self):
        scenario = (HERE / "scenarios.sh").read_text()
        check = re.search(r"^balanced\(\) \{.*?^}", scenario, re.M | re.S).group()
        alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
        def accepted(values, order=None):
            with tempfile.TemporaryDirectory() as tmp:
                urls = []
                expected = []
                for i, value in enumerate(values):
                    name = "".join(alphabet[(value >> n) & 63] for n in reversed(range(0, 96, 6)))
                    expected.append(name)
                    host = Path(tmp) / str(i)
                    host.mkdir()
                    (host / "metrics").write_text(json.dumps({"name": name}))
                    urls.append(host.as_uri())
                cmd = check + '\nMONITORS=$1; FOUNDER_MONITORS=$3; balanced "$2" "$4"'
                ordered = [urls[i] for i in order] if order is not None else []
                return subprocess.run(["bash", "-uc", cmd, "_", ",".join(urls), str(len(urls)),
                                       ",".join(ordered), ",".join(expected) if ordered else ""],
                                      capture_output=True).returncode == 0
        high = 1 << 95
        self.assertTrue(accepted([0, 1, high, high | (1 << 93)]))  # four quarter cells
        self.assertFalse(accepted([*range(8), high, high + 1]))  # sixteenths and quarters
        self.assertFalse(accepted([0, 0]))
        self.assertTrue(accepted([0, high], [0, 1]))
        self.assertFalse(accepted([0, high], [1, 0]))  # same names on opposite hosts

    def test_steps_stop_before_load_on_rejected_layout(self):
        script = (HERE / "scenarios.sh").read_text()
        steps = re.search(r"^steps\(\) \{.*?^}", script, re.M | re.S).group()
        harness = 'cluster() { return 1; }; phase() { echo unexpected; }; PROVISIONER=fake STEP_SEED=1 SIZES=8 steps'
        run = subprocess.run(["bash", "-uc", steps + "\n" + harness], capture_output=True, text=True)
        self.assertNotEqual(run.returncode, 0)
        self.assertEqual(run.stdout, "")

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
