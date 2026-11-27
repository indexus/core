"""Reproduce the November 2026 offline placement projection from adjacent aggregates."""
import argparse
import csv
import json
import math
from bisect import bisect_right
from dataclasses import dataclass, replace
from pathlib import Path

import matplotlib
import numpy as np
from scipy.optimize import least_squares


matplotlib.use("Agg")
import matplotlib.pyplot as plt

HERE = Path(__file__).resolve().parent
STEADY_CPU = 50
COPY_ADMISSION_CPU = 60
RESERVED_RECEIVER_CPU = 100 * 1.75 / 4
COOLDOWN_SECONDS = 180


def read_csv(name):
    with (HERE / name).open() as stream:
        return list(csv.DictReader(stream))


def write_csv(path, rows):
    with path.open("w", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=rows[0], lineterminator="\n")
        writer.writeheader()
        writer.writerows(rows)


@dataclass(frozen=True)
class CPU:
    idle: float
    per_vnode: float
    per_write: float

    def uniform_density(self, writes_per_vnode):
        return (STEADY_CPU - self.idle) / (self.per_vnode + self.per_write * writes_per_vnode)


def calibrate(idle, rows):
    rates = np.array([[int(r["vnodes"]), float(r["appliquees_s"])] for r in rows])
    used = np.array([float(r["cpu_pct"]) - idle for r in rows])
    fit = least_squares(
        lambda costs: rates @ costs - used,
        [1.0, 0.03],
        bounds=(0, np.inf),
        loss="soft_l1",
        f_scale=2,
    )
    return CPU(idle, *fit.x)


@dataclass(frozen=True)
class Scenario:
    name: str
    threshold: int = 50
    boot: int = 45
    copy: int = 10
    receive_cpu: float = 0
    send_cpu: float = 8
    fraction: float = 0.5
    max_hosts: int | None = None
    slots: int | None = None
    reserved_cpu: bool = False
    catchup: int | None = None


def scenarios(config):
    # Convert receiver CPU usage in cores to percentage points of a four-core host.
    typical = Scenario("50-central", receive_cpu=config["receiver_cpu_cores_median"] * 25)
    lanes = replace(typical, name="50-voies", catchup=120)
    strict = replace(lanes, name="50-voies-strict", max_hosts=32, reserved_cpu=True)
    slow = replace(
        strict, name="50-voies-longues", boot=75, copy=36, send_cpu=15, fraction=0.4,
        receive_cpu=config["receiver_cpu_cores_p90"] * 25,
    )
    return [
        typical, lanes, replace(lanes, name="50-voies-32", max_hosts=32),
        replace(lanes, name="50-voies-24", max_hosts=24), strict, slow,
        replace(typical, name="50-desequilibre", fraction=0.4),
        replace(slow, name="50-tardif", max_hosts=None, catchup=None),
        replace(typical, name="50-copies-courtes", boot=30, copy=6),
        replace(typical, name="100-cinq-slots", threshold=100, copy=30, receive_cpu=19,
                send_cpu=12, max_hosts=32, slots=5, reserved_cpu=True),
    ]


def binary_target(demand, shares, case):
    rates = [demand * share for _, share in shares]
    leaves = 0
    while rates:
        rate = rates.pop()
        if rate <= case.threshold:
            leaves += 1
        else:
            rates.extend([rate * case.fraction, rate * (1 - case.fraction)])
    return leaves


def desired_hosts(target, demand, vnodes, case, cpu):
    steady_cost = target * cpu.per_vnode + demand * cpu.per_write
    available = STEADY_CPU - cpu.idle
    desired = math.ceil(steady_cost / available)
    if case.slots:
        desired = max(desired, math.ceil(target / case.slots))
    if case.catchup:
        donors = {h for h, share in vnodes if share * demand > case.threshold}
        missing = max(0, target - len(vnodes))
        lanes = min(len(donors), math.ceil(missing * case.copy / case.catchup))
        with_copies = steady_cost + lanes * (case.receive_cpu + case.send_cpu)
        desired = max(desired, lanes * 2, math.ceil(with_copies / available))
    return desired


@dataclass(frozen=True)
class Copy:
    finish: int
    vnode: int
    destination: int
    fraction: float


def simulate(case, cpu, initial, demand_by_second, control=None):
    active = initial.copy()
    host_ready_at = [0] * len({host for host, _ in active})
    copies = []
    backlog = {limit: {} for limit in (50, 100)}
    rows = []

    for t, demand in enumerate(demand_by_second):
        for copy in copies:
            if copy.finish == t:
                source, share = active[copy.vnode]
                active[copy.vnode] = (source, share * (1 - copy.fraction))
                active.append((copy.destination, share * copy.fraction))
        copies = [copy for copy in copies if copy.finish > t]

        target = binary_target(demand, initial, case) if demand else 0
        desired = desired_hosts(target, demand, active, case, cpu) if demand else 0
        if t % 5 == 0 and demand:
            planned = min(desired, case.max_hosts) if case.max_hosts else desired
            host_ready_at.extend([t + case.boot] * max(0, planned - len(host_ready_at)))

        host_count = len(host_ready_at)
        traffic = np.zeros(host_count)
        slots = np.zeros(host_count)
        copy_cpu = np.zeros(host_count)
        busy = set()
        for host, share in active:
            traffic[host] += share * demand
            slots[host] += 1
        for copy in copies:
            source, share = active[copy.vnode]
            moved = share * demand * copy.fraction * (1 - (copy.finish - t) / case.copy)
            traffic[source] -= moved
            traffic[copy.destination] += moved
            slots[copy.destination] += 1
            copy_cpu[source] += case.send_cpu
            copy_cpu[copy.destination] += case.receive_cpu
            busy.update((source, copy.destination))

        used = cpu.idle + slots * cpu.per_vnode + traffic * cpu.per_write
        ready = sum(available <= t for available in host_ready_at)
        over = sum(share * demand > case.threshold + 1e-8 for _, share in active)
        # Keep placement and data-plane admission identical across variants.
        reserve = RESERVED_RECEIVER_CPU if case.reserved_cpu else case.receive_cpu

        def destinations(vnode):
            source, share = active[vnode]
            rate = share * demand
            if rate <= case.threshold or source in busy or used[source] + case.send_cpu > COPY_ADMISSION_CPU:
                return []
            return [host for host in range(ready)
                    if host != source and host not in busy
                    and (not case.slots or slots[host] < case.slots)
                    and used[host] + reserve + cpu.per_vnode <= COPY_ADMISSION_CPU
                    and used[host] + cpu.per_vnode + rate * case.fraction * cpu.per_write <= STEADY_CPU]

        def start_copy(vnode, destination):
            source, _ = active[vnode]
            fraction = control.random.choice(control.fractions) if control else case.fraction
            copies.append(Copy(t + case.copy, vnode, destination, fraction))
            busy.update((source, destination))
            slots[destination] += 1
            used[destination] += cpu.per_vnode
            copy_cpu[source] += case.send_cpu
            copy_cpu[destination] += case.receive_cpu

        copying = {copy.vnode for copy in copies}
        hot = {v for v, (_, share) in enumerate(active) if share * demand > case.threshold} - copying
        if control and control.offers_first:
            settled = control.settle(t)
            for _, source, destination, _ in control.reservations.values():
                busy.update((source, destination))
            for vnode, destination in settled:
                if destination in destinations(vnode):
                    start_copy(vnode, destination)
                    del control.waiting[vnode]
                    hot.discard(vnode)
                else:
                    control.counts["local_admission_blocked"] += 1
            if t % 2 == 0:
                control.update(hot, t)
                waiting = hot - control.reservations.keys()
                for reason in ("source_busy", "source_cpu", "no_receiver"):
                    control.counts["waiting_" + reason] = 0
                for vnode in sorted(waiting, key=lambda v: (control.waiting[v], -active[v][1], v)):
                    source, _ = active[vnode]
                    candidates = destinations(vnode)
                    if candidates:
                        dest = min(candidates, key=lambda h: (traffic[h], slots[h], h))
                        control.reserve(vnode, source, dest, t)
                        busy.update((source, dest))
                    elif source in busy:
                        control.counts["waiting_source_busy"] += 1
                    elif used[source] + case.send_cpu > COPY_ADMISSION_CPU:
                        control.counts["waiting_source_cpu"] += 1
                    else:
                        control.counts["waiting_no_receiver"] += 1
        else:
            eligible = control.advance(t) if control else (hot if t % 2 == 0 else set())
            if control and t % 2 == 0:
                for vnode in sorted(hot - eligible):
                    control.submit(vnode, t)
            for vnode in sorted(eligible, key=lambda v: (-active[v][1], v)):
                candidates = destinations(vnode)
                if not candidates:
                    if control:
                        control.counts["local_admission_blocked"] += 1
                    continue
                destination = min(candidates, key=lambda h: (traffic[h], slots[h], h))
                start_copy(vnode, destination)

        for limit, queue in backlog.items():
            for host in range(ready):
                spare = limit - cpu.idle - cpu.per_vnode * slots[host] - copy_cpu[host]
                service = max(0, spare / cpu.per_write)
                queue[host] = max(0, queue.get(host, 0) + traffic[host] - service)

        rows.append(dict(
            variante=case.name, t=t, demande=demand, ideal_vnodes=math.ceil(demand / case.threshold),
            cible_binaire=target, ideal_hotes=desired, vnodes=len(active), hotes_prets=ready,
            hotes_lances=host_count, splits_en_cours=len(copies), vnodes_au_dessus=over,
            pic_vnode_s=max(share * demand for _, share in active),
            charge_cpu_pic_pct=max((used + copy_cpu)[:ready]),
            attente_cap50=sum(backlog[50].values()), attente_cap100=sum(backlog[100].values()),
        ))
        if control:
            rows[-1].update(control.counts)
            if control.offers_first:
                rows[-1].update(control.backlog(t))
    return rows


def draw(output, primary, cpu, load_end):
    observed = read_csv("observations100.csv")
    legacy = read_csv("ancien-modele.csv")
    fig, axes = plt.subplots(2, 1, figsize=(11, 8), sharex=True, layout="constrained")
    fig.suptitle("Combien de vnodes et de machines pendant une montée à 16 000 écritures/s ?", fontsize=16)
    seconds = [r["t"] for r in primary]
    for ax, field, count, title in zip(
        axes, ("vnodes", "hotes_prets"), ("vnodes", "hotes"), ("Vnodes installés", "Machines prêtes"),
    ):
        ax.plot(seconds, [r[field] for r in primary], color="#126b70", linewidth=2.5,
                label="Simulation 50/s · admission actuelle · plafond 32")
        ax.plot([int(r["t"]) for r in observed], [int(r[count]) for r in observed],
                color="#cf691e", linewidth=2.5, label="AWS observé · seuil 100/s")
        ax.scatter([int(r["t"]) for r in legacy], [int(r[count]) for r in legacy],
                   marker="D", color="#534968", s=38, label="Ancien modèle 100/s · 5 places · plafond 32", zorder=5)
        ax.axvspan(load_end, seconds[-1], color="#e3e7ea", alpha=.65)
        ax.axvline(load_end, color="#647780", linewidth=1)
        ax.set(ylabel=title, xlim=(0, seconds[-1]))
        ax.grid(axis="y", alpha=.22)
        ax.spines[["top", "right"]].set_visible(False)
    axes[0].plot(seconds, [r["ideal_vnodes"] for r in primary], color="#777c85", linestyle="--",
                 label="Minimum uniforme · 1 vnode par 50/s")
    axes[0].plot(seconds, [r["cible_binaire"] for r in primary], color="#525c90", linestyle=":",
                 label="Subdivisions binaires nécessaires · sans délai")
    axes[0].text(load_end + 10, 455, "Fin des arrivées\nLes données restent en place.", fontsize=9, color="#42515b")
    for x, y, label in [(15, 118, "2k/s"), (225, 180, "4k/s"), (402, 305, "8k/s"), (560, 545, "16k/s")]:
        axes[0].text(x, y, label, fontsize=9)
    axes[0].set_ylim(0, 630)
    floor = [math.ceil((r["cible_binaire"] * cpu.per_vnode + r["demande"] * cpu.per_write) /
                       (STEADY_CPU - cpu.idle)) for r in primary]
    axes[1].plot(seconds, floor, color="#777c85", linestyle="--", label="Plancher CPU · trafic et fond des vnodes binaires")
    axes[1].set_ylim(0, 39)
    axes[1].set_xlabel("Temps depuis le début de la charge (s) · refroidissement simulé après 660 s")
    for ax in axes:
        ax.legend(fontsize=8, loc="upper left", ncols=2)
    fig.savefig(output / "granularite50.png", dpi=145)
    fig.savefig(output / "granularite50.pdf", metadata={"CreationDate": None})
    plt.close(fig)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=HERE, help="directory for the projections and plots")
    parser.add_argument("--series", action="store_true", help="also export all 841 seconds of each scenario")
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)

    cfg = json.loads((HERE / "campagne.json").read_text())
    measurements = read_csv("calibrage.csv")
    cpu = calibrate(cfg["idle_cpu_pct"], measurements)
    hosts = {r["hote"] for r in measurements}
    densities = [calibrate(cpu.idle, [r for r in measurements if r["hote"] != h]).uniform_density(50)
                 for h in sorted(hosts)]
    print(f"{len(measurements)} quiet host samples; density at 50/s = {cpu.uniform_density(50):.2f}; "
          f"leave-one-host-out range = {min(densities):.2f} to {max(densities):.2f}")

    total = sum(rate for _, rate in cfg["initial_host_and_wps"])
    initial = [(host, rate / total) for host, rate in cfg["initial_host_and_wps"]]
    load_end = cfg["end_load_s"]
    changes = cfg["offered_wps_changes_at_second"]
    starts = [t for t, _ in changes]
    demand = [changes[bisect_right(starts, max(1, t)) - 1][1] if t <= load_end else 0
              for t in range(load_end + COOLDOWN_SECONDS + 1)]

    variants = {case.name: simulate(case, cpu, initial, demand) for case in scenarios(cfg)}
    summary = []
    for name, rows in variants.items():
        last = rows[load_end]
        summary.append(dict(variante=name, demande_660=last["demande"], vnodes_660=last["vnodes"],
                            hotes_660=last["hotes_prets"], copies_660=last["splits_en_cours"],
                            au_dessus_660=last["vnodes_au_dessus"], pic_vnode_660=round(last["pic_vnode_s"], 2),
                            pic_file_50=round(max(r["attente_cap50"] for r in rows)),
                            pic_file_100=round(max(r["attente_cap100"] for r in rows)),
                            vnodes_840=rows[-1]["vnodes"], hotes_840=rows[-1]["hotes_prets"]))
    write_csv(args.output / "resultats.csv", summary)
    if args.series:
        write_csv(args.output / "simulation.csv", [r for rows in variants.values() for r in rows])
    draw(args.output, variants["50-voies-strict"], cpu, load_end)


if __name__ == "__main__":
    main()
