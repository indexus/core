"""Repeat the AWS 50/s scenario with sampled scale-request outcomes and delays."""
import argparse
from bisect import bisect_right
import csv
import json
from pathlib import Path
from statistics import median

import matplotlib.pyplot as plt

from projection import HERE, calibrate, read_csv, scenarios, simulate, write_csv
from request_lifecycle import OfferQueue, RequestLifecycle


def run(observations, output, seeds):
    training = json.loads((HERE / "control-training.json").read_text())
    config = json.loads((HERE / "campagne.json").read_text())
    case = next(case for case in scenarios(config) if case.name == "50-voies-strict")
    cpu = calibrate(config["idle_cpu_pct"], read_csv("calibrage.csv"))
    initial_rates = config["initial_host_and_wps"]
    total = sum(rate for _, rate in initial_rates)
    initial = [(host, rate / total) for host, rate in initial_rates]
    changes = config["offered_wps_changes_at_second"]
    starts = [t for t, _ in changes]
    demand = [changes[bisect_right(starts, max(1, t)) - 1][1] for t in range(config["end_load_s"] + 1)]
    models = {"instant": [simulate(case, cpu, initial, demand)]}
    for name in ("synchrone", "ticket"):
        models[name] = [simulate(case, cpu, initial, demand, RequestLifecycle(training, seed, name == "ticket"))
                        for seed in range(seeds)]
    models["offre_reservee"] = [simulate(case, cpu, initial, demand, OfferQueue(training, seed))
                                for seed in range(seeds)]
    with observations.open() as stream:
        observed = list(csv.DictReader(stream))
    derived = []
    errors = []
    counters = []
    for name, runs in models.items():
        for point in observed:
            t = int(point["t"])
            if not 0 <= t < len(demand):
                continue
            row = dict(t=t, model=name)
            for column, real in [("vnodes", "ready"), ("hotes_prets", "hosts"), ("pic_vnode_s", "hottest_vnode"),
                                 ("charge_cpu_pic_pct", "host_cpu_max")]:
                row[real] = float(point[real]) if point[real] else None
                values = sorted(samples[t][column] for samples in runs)
                row[real + "_pred"] = round(median(values), 2)
                row[real + "_p10"] = round(values[int(0.10 * (len(values) - 1))], 2)
                row[real + "_p90"] = round(values[int(0.90 * (len(values) - 1))], 2)
            derived.append(row)
        held_out = [r for r in derived if r["model"] == name and r["t"] > training["training_end_s"]]
        for field in ("ready", "hosts", "hottest_vnode", "host_cpu_max"):
            if name not in ("instant", "synchrone"):
                continue
            errors_at_t = [abs(r[field] - r[field + "_pred"]) for r in held_out if r[field] is not None]
            errors.append(dict(model=name, field=field, n=len(errors_at_t),
                               mean_absolute_error=round(sum(errors_at_t) / len(errors_at_t), 2)))
        if name != "instant":
            keys = ["requests", "client_timeouts", "server_refusals", "ambiguous_refusals",
                    "local_admission_blocked"]
            if name == "offre_reservee":
                keys += ["reservation_attempts", "waiting_intents", "reserved_intents", "oldest_waiting_s"]
                keys += ["waiting_source_busy", "waiting_source_cpu", "waiting_no_receiver"]
            for key in keys:
                counters.append(dict(model=name, counter=key, median=median(r[-1].get(key, 0) for r in runs)))
    output.mkdir(parents=True, exist_ok=True)
    write_csv(output / "control-replay.csv", derived)
    write_csv(output / "control-errors.csv", errors)
    write_csv(output / "control-counts.csv", counters)
    fig, axes = plt.subplots(2, 1, figsize=(10, 7), constrained_layout=True, sharex=True)
    labels = {"instant": "Ancien modèle", "synchrone": "Demandes et délais", "offre_reservee": "Offres réservées (hypothèse)"}
    for ax, field, title in zip(axes, ("ready", "hottest_vnode"),
                                ("Vnodes prêts", "Débit du vnode le plus chargé (écritures/s)")):
        aws = [r for r in derived if r["model"] == "instant"]
        ax.plot([r["t"] for r in aws], [r[field] for r in aws], color="black", label="AWS", lw=2.2)
        for name, label in labels.items():
            rows = [r for r in derived if r["model"] == name]
            times = [r["t"] for r in rows]
            ax.plot(times, [r[field + "_pred"] for r in rows], label=label)
            if name == "synchrone":
                ax.fill_between(times, [r[field + "_p10"] for r in rows], [r[field + "_p90"] for r in rows],
                                alpha=.16, color="tab:orange", label="p10–p90 des graines")
        ax.axvspan(0, training["training_end_s"], color="grey", alpha=.08)
        ax.axvline(training["training_end_s"], linestyle=":", color="black")
        ax.set_ylabel(title)
        ax.grid(axis="y", alpha=.2)
    axes[0].legend(ncols=3, fontsize=8)
    axes[-1].set_xlabel("Temps depuis le début de la charge (s) ; calibrage jusqu’à 300 s")
    fig.savefig(output / "control-replay.pdf", metadata={"CreationDate": None})
    plt.close(fig)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("observations", type=Path, help="synchronized dashboard comparison CSV")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--seeds", type=int, default=20)
    args = parser.parse_args()
    if args.seeds < 1:
        parser.error("--seeds must be positive")
    run(args.observations, args.output, args.seeds)
