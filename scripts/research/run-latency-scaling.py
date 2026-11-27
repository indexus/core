#!/usr/bin/env python3
"""Sequential local lab runs plus independent macOS resource/fsync telemetry.
No dependencies or remote machines. All stores share this host's physical SSD.
"""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


def output(command):
    return subprocess.check_output(command, text=True, stderr=subprocess.DEVNULL)


def cpu_seconds(value):
    parts = value.split(":")
    return sum(float(v) * 60**i for i, v in enumerate(reversed(parts)))


def host_sample():
    raw = output(["top", "-l", "2", "-s", "1", "-n", "0"])
    cpus = re.findall(r"CPU usage: ([\d.]+)% user, ([\d.]+)% sys, ([\d.]+)% idle", raw)
    pressure = output(["memory_pressure", "-Q"])
    free = re.search(r"free percentage: (\d+)%", pressure)
    vm = output(["vm_stat"])
    pages = {}
    for key in ("Swapins", "Swapouts", "Pageouts", "Pages occupied by compressor"):
        match = re.search(re.escape(key) + r":\s+(\d+)", vm)
        if match:
            pages[key] = int(match[1])
    network = {}
    lines = output(["netstat", "-ibn", "-I", "lo0"]).splitlines()
    if lines:
        for line in lines[1:]:
            columns = line.split()
            if columns and columns[0] == "lo0":
                network = {"Ibytes": int(columns[-5]), "Obytes": int(columns[-2])}
                break
    return dict(
        cpu_user_pct=float(cpus[-1][0]),
        cpu_system_pct=float(cpus[-1][1]),
        cpu_idle_pct=float(cpus[-1][2]),
        memory_free_pct=int(free[1]),
        vm_pages=pages,
        loopback=network,
    )


def sample(pid, probe, previous):
    at = time.monotonic()
    row = dict(unix_ns=time.time_ns())
    if pid:
        try:
            values = output(["ps", "-p", str(pid), "-o", "time=,rss="]).split()
            used = cpu_seconds(values[0])
            row.update(process_cpu_s=used, rss_bytes=int(values[1]) * 1024)
            if previous:
                row["process_cpu_pct"] = 100 * (used - previous[1]) / (at - previous[0])
            previous = at, used
        except subprocess.CalledProcessError:
            pass  # the test can finish between poll and ps
    os.pwrite(probe, str(row["unix_ns"]).encode(), 0)
    began = time.monotonic()
    fcntl.fcntl(probe, fcntl.F_FULLFSYNC)
    row["probe_fullfsync_us"] = round((time.monotonic() - began) * 1e6)
    return row, previous


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--binary", type=Path, required=True)
    p.add_argument("--out", type=Path, required=True)
    p.add_argument("--binary-commit", help="production commit represented by the measured binary")
    p.add_argument("--nodes", type=int, nargs="+", default=[4, 8, 16, 32, 64])
    p.add_argument("--rate", type=int, default=40, help="constant aggregate writes/s, not per-node")
    p.add_argument("--phase-seconds", type=int, default=10, help="four phases at the same rate")
    p.add_argument("--seed-zones", type=int, default=2048)
    p.add_argument("--store-memory-mib", type=int, default=2, help="same budget for each store")
    p.add_argument("--warmup-seconds", type=int, default=10)
    p.add_argument("--dynamic", action="store_true", help="joins during load, then same-rate tail")
    p.add_argument("--faults", action="store_true")
    p.add_argument(
        "--no-maintenance", action="store_true", help="diagnostic ablation, not production default"
    )
    p.add_argument(
        "--alloc-profile", action="store_true", help="dump default sampled allocations after test"
    )
    p.add_argument("--repeats", type=int, default=1)
    args = p.parse_args()
    if args.store_memory_mib < 1:
        p.error("--store-memory-mib must be positive")
    if sys.platform != "darwin":
        p.error("this observer uses macOS top/vm_stat/F_FULLFSYNC")
    if args.out.exists() and any(args.out.iterdir()):
        p.error("--out must be a new empty directory")
    args.out.mkdir(parents=True, exist_ok=True)
    probe_path = args.out / "fsync-probe"
    probe = os.open(probe_path, os.O_RDWR | os.O_CREAT, 0o600)
    manifest = dict(
        command=sys.argv,
        cpu_count=os.cpu_count(),
        host_memory_bytes=int(output(["sysctl", "-n", "hw.memsize"])),
        source_commit=args.binary_commit or output(["git", "rev-parse", "HEAD"]).strip(),
        checkout_commit=output(["git", "rev-parse", "HEAD"]).strip(),
        binary_sha256=hashlib.sha256(args.binary.read_bytes()).hexdigest(),
        runs=[],
        note="single physical SSD and CPU; aggregate load/data held fixed; not EC2 capacity",
    )
    (args.out / "working-tree.patch").write_text(output(["git", "diff"]))
    with (args.out / "baseline-resources.jsonl").open("w") as f:
        for _ in range(3):
            row, _ = sample(None, probe, None)
            row.update(host_sample())
            f.write(json.dumps(row) + "\n")
    try:
        for repeat in range(args.repeats):
            for nodes in args.nodes:
                name = f"{'dynamic' if args.dynamic else 'static'}-{nodes:02d}-r{repeat + 1}"
                run = args.out / name
                run.mkdir()
                env = dict(
                    os.environ,
                    GOMAXPROCS="8",
                    INDEXUS_LAB64="1",
                    INDEXUS_BENCH_NO_USAGE_SCAN="1",
                    INDEXUS_LAB_NODES=str(nodes),
                    INDEXUS_LAB_PREJOIN="0" if args.dynamic else "1",
                    INDEXUS_LAB_DIRECT="1",
                    INDEXUS_LAB_UNIQUE_ZONES="1",
                    INDEXUS_LAB_WRITE_TRACE="1",
                    INDEXUS_LAB_STACKS="0",
                    INDEXUS_LAB_MAINTENANCE="0" if args.no_maintenance else "1",
                    INDEXUS_LAB_FAULTS="1" if args.faults else "0",
                    INDEXUS_LAB_RATE=str(args.rate),
                    INDEXUS_LAB_TAIL_RATE=str(args.rate) if args.dynamic else "0",
                    INDEXUS_LAB_PHASE_SECONDS=str(args.phase_seconds),
                    INDEXUS_LAB_SEED_ZONES=str(args.seed_zones),
                    INDEXUS_LAB_STORE_MEMORY_MIB=str(args.store_memory_mib),
                    INDEXUS_LAB_WARMUP_SECONDS=str(args.warmup_seconds),
                    INDEXUS_LAB_TIMEOUT_SECONDS="600",
                    INDEXUS_LAB_OUT=str(run.resolve()),
                )
                cmd = [
                    str(args.binary.resolve()),
                    "-test.run=^Test64NodeDurableHTTPRecovery$",
                    "-test.count=1",
                    "-test.v",
                    "-test.timeout=12m",
                ]
                if args.alloc_profile:
                    cmd.append("-test.memprofile=" + str((run / "allocs.pprof").resolve()))
                record = dict(
                    name=name,
                    nodes=nodes,
                    start_ns=time.time_ns(),
                    env={
                        k: v
                        for k, v in env.items()
                        if k.startswith("INDEXUS_LAB") or k in ("GOMAXPROCS", "GOGC", "GOMEMLIMIT")
                    },
                )
                print("starting", name, flush=True)
                with (
                    (run / "go-test.log").open("w") as log,
                    (run / "resources.jsonl").open("w") as resources,
                ):
                    process = subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT, env=env)
                    record["pid"] = process.pid
                    last, tick = None, 0
                    try:
                        while process.poll() is None:
                            began = time.monotonic()
                            row, last = sample(process.pid, probe, last)
                            if tick % 5 == 0:
                                row.update(host_sample())
                            resources.write(json.dumps(row) + "\n")
                            resources.flush()
                            tick += 1
                            time.sleep(max(0, 2 - (time.monotonic() - began)))
                    except BaseException:
                        process.terminate()
                        process.wait(timeout=30)
                        raise
                    record.update(exit_code=process.wait(), finish_ns=time.time_ns())
                manifest["runs"].append(record)
                (args.out / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
                print("finished", name, record["exit_code"], flush=True)
                if record["exit_code"]:
                    raise SystemExit("lab failed; logs retained, no later run launched")
    finally:
        os.close(probe)


if __name__ == "__main__":
    main()
