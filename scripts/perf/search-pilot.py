#!/usr/bin/env python3
"""Exercise local search oracles and measure the storage fingerprint hot/cold path."""
import argparse
import json
import os
import platform
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[2]
HTTP_TESTS = [
    "TestHybridPayloadsSDKTwoColdPeers", "TestSDKFilteredKnnTwoColdPeers",
    "TestSDKRangeAggregatesTwoColdPeers", "TestArrayFiltersSDKTwoPeers",
    "TestSDKSelectiveSubtreeRefresh",
]


def output(cmd, cwd):
    return subprocess.check_output(cmd, cwd=cwd, text=True).strip()


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("sdk", type=Path, help="prepared Indexus JS SDK checkout")
    ap.add_argument("results", type=Path, help="new output directory")
    args = ap.parse_args()
    sdk = args.sdk.resolve()
    results = args.results.resolve()
    results.mkdir(parents=True, exist_ok=False)
    env = {**os.environ, "INDEXUS_TEST_SDK": str(sdk), "GOPROXY": "off"}
    manifest = {
        "core": output(["git", "rev-parse", "HEAD"], ROOT),
        "sdk": output(["git", "rev-parse", "HEAD"], sdk),
        "core_changes": output(["git", "status", "--porcelain"], ROOT),
        "sdk_changes": output(["git", "status", "--porcelain"], sdk),
        "host": platform.platform(), "arch": platform.machine(), "logical_cpus": os.cpu_count(),
        "go": output(["go", "version"], ROOT), "node": output(["node", "--version"], sdk),
        "commands": [],
    }

    def run_log(name, command, cwd):
        manifest["commands"].append({"cwd": str(cwd), "argv": command, "log": name})
        (results / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        with (results / name).open("w") as log:
            subprocess.run(command, cwd=cwd, env=env, stdout=log,
                           stderr=subprocess.STDOUT, check=True)

    run_log("bitmaps.tap", ["node", "--test", "src/walk/documentUniverse.test.mjs",
                             "src/walk/presence.test.mjs"], sdk)
    run_log("bitmap-count.json", ["node", "scripts/perf/search-bitmaps.mjs", str(sdk), "100000"], ROOT)
    selection = "^(" + "|".join(HTTP_TESTS) + ")$"
    run_log("two-peers.jsonl", ["go", "test", "-json", "-count=1", "-run", selection,
                                "./internal/app/node"], ROOT)
    events = [json.loads(line) for line in (results / "two-peers.jsonl").read_text().splitlines()]
    passed = {e.get("Test") for e in events if e["Action"] == "pass"}
    missing = set(HTTP_TESTS) - passed
    if missing:
        raise RuntimeError(f"Expected passing HTTP tests, missing/skipped: {sorted(missing)}")
    run_log("fingerprint.txt", ["go", "test", "-run=^$", "-bench=^BenchmarkLocalSubtreeFingerprint$",
                                "-benchmem", "-benchtime=200ms", "-count=3", "./internal/storage"], ROOT)
    bench = (results / "fingerprint.txt").read_text()
    for mode in ["cold-result-cache", "hot-result-cache"]:
        rows = [line for line in bench.splitlines() if line.startswith("BenchmarkLocalSubtreeFingerprint/" + mode)]
        if len(rows) != 3 or any("keys/op" not in row for row in rows):
            raise RuntimeError(f"Expected three measured {mode} runs")
    print(f"{len(HTTP_TESTS)} two-peer scenarios passed. Logs and storage measurements: {results}")


if __name__ == "__main__":
    main()
