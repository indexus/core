# Resource sizing lab

This deterministic resource model is separate from the Go protocol simulator.
It cannot prove authority safety or measure client latency. Its outputs label
workload demand, not synthetic successful ACKs. It shares the fleet renderer
with the AWS experiment dashboard, not a second copy of that renderer.

```bash
node --test ops/experiment/sizing/model.test.mjs
node ops/experiment/sizing/sweep.mjs /absolute/results
node ops/experiment/sizing/build-view.mjs /absolute/sizing.html --page
```

Open the resulting self-contained page in a browser. Omit `--page` to embed the
fragment in a host which supplies the visualization theme. All assets are
embedded; no telemetry is uploaded and no AWS resources are created.

The controls compare slot limits, trigger percentages, copy budgets, reserve
percentages, write cost, data density, boot time and collection policy. Copies
move load progressively; dirty work, resource excess and unfinished copies are
visible. A fixed `warmHosts` override exists only for explicit controlled tests;
the default and UI use a resource percentage.

See [assumptions, 465 cases and conclusions](../../../docs/benchmarks/resource-sizing-2026-11-26.md)
and [the common telemetry contract](../dashboard/README.md).


## Execution counterexamples after the AWS failures

The fluid `model.mjs` models an intended resource policy; it does **not** run
the production controllers. In particular it budgets copies before they run,
distributes load with copy progress, and frees garbage after a delay without
charging the writer or CPU. Its 64 initial vnodes, 16-GiB default and 60-second
ramps are not the AWS run's four initial vnodes, 32-GiB hosts and 120-second ramps.
Do not use that sweep to forecast AWS ACK throughput or p99.

`queues.mjs` adds a separate, small execution layer for counterexamples:

- physical CPU shared by co-resident vnodes;
- one serial writer per vnode, including maintenance that holds it;
- shared I/O lanes and blocking time, configured explicitly;
- finite client workers/queue, refusal, unfinished work and intended latency;
- maintenance debt that remains visible even when foreground ACKs look good;
- ready nodes with no traffic until an externally supplied installed-share event.

```sh
node --test ops/experiment/sizing/queues.test.mjs
node ops/experiment/sizing/run-queues.mjs /absolute/queues-results
```

Memory is bounded by worker/queue limits, one active job per background source,
fixed histograms and the declared network. Counters retain deferred maintenance
work without allocating one object per missed timer. There are explicit scope
limits on steps, network size and total offered requests.

The runner uses 64 model vnode identities on 32 declared hosts, of which only
8 initially have traffic shares. Its costs are **synthetic**, not calibrated.
Two timesteps check discretization sensitivity. `stepsRate` provides the AWS
2k→4k→8k→16k workload clock with 120-second ramps for future trace replays.

This layer does not execute handoff, route repair, group commit, the actual
provisioner, boot reservations, CRDTs or durability. Its I/O lanes are assumptions,
not an NVMe model. Installation events are inputs, not permissions created by
the simulator. Successful synthetic tasks are not proof of real durable ACKs.
Report ACKs, losses, unfinished requests **and maintenance debt** together.

See [the measured missing cases and calibration requirements](../../../docs/benchmarks/simulation-gaps-2026-11-26.md).

## Calibration partielle à partir du banc multi-processus

Le [banc Linux local](../multiprocess/README.md) publie les temps de tenue du
writer par nature de transaction. `calibrate-queues.mjs` charge ces mesures et
les observations des tests de travail, avec leurs empreintes SHA-256. Il vérifie
la loi de la fixture réservée `reads = 8 + receipts * (sessions + 1)` et garde
séparés les trois points mesurés après installation (64 reçus, 71 lectures).
Il ne transforme pas les lectures logiques en microsecondes universelles.

`writeHoldMs` et `background[].holdMs` représentent une durée murale mesurée
sous le writer. Ils ne consomment pas artificiellement du CPU ou un canal disque.
Les replays de calibration utilisent des moyennes et une répartition égale
explicitement supposée ; ils n'annoncent pas un p99 prédit. `calibrated` reste
`false` tant que les coûts CPU/I/O, les distributions de service, les événements
de placement et les bascules réelles de charge ne sont pas qualifiés ensemble.

## Rejouer les deux bancs EBS 64k

Depuis ce dossier, `python3 placement_replay.py RESULTS` rejoue les décisions
échantillonnées dans `operator/placement.jsonl`. Un octroi distant réellement
appliqué fournit un créneau sur la machine choisie. Le mode observé restitue
les bénéficiaires. Quatre variantes réattribuent ce créneau selon l’âge, le
débit, leur produit au-dessus du seuil, ou la pression CPU du donneur. Une
alternative exige un inventaire récent, une destination distante et des items
connus qui satisfont la règle de soulagement. Un octroi par couple donneur/préfixe.
Les octrois locaux, les décisions non échantillonnées et les annulations ne sont
pas reconstruits. Les attentes non servies sont censurées à la fin de la fenêtre.
Tester `--lease 15`, `30` et `60` mesure la sensibilité aux renouvellements.
Les prochaines demandes et les créneaux restent ceux de la trace, même après
une réattribution hypothétique. Un créneau octroyé précède le transfert effectif.

`python3 fleet_replay.py RESULTS --boot 45` compare trois boucles sur les trames
CPU complètes. Elles utilisent la capacité prête, la capacité prête plus engagée,
ou une anticipation du trafic sur le délai de boot, bornée à un doublement.
La charge CPU observée est maintenue fixe. L’intégrale du manque de capacité
mesure le dépassement de la cible moyenne, en équivalents machine-seconde.
Une machine en chauffe compte dans le coût dès sa demande. Les durées 20, 45 et
90 secondes sont des scénarios. L’interpolation est constante entre trames.

`python3 zone_service.py TRAIN/live.jsonl TEST/live.jsonl` mesure une vitesse par
opération sur deux observations successives, et compare une prédiction constante
à une estimation révisée après chaque observation (constante de temps 10 s).
Pour une seule capture, fournir deux fois le chemin et `--split-time 400` pour
réserver la période après charge aux tests, avec des opérations distinctes.
Les opérations achevées entre deux sondes et celles de moins de dix secondes
échappent à cette mesure. Le compteur installed_zones manque sur le premier banc EBS.

Le modèle en boucle fermée accepte `scenario: 'aws64k'`, `initialHosts: 12`,
`initialVnodes: 48`, `splitRate: 50`, ainsi que `bootPolicy: 'forecast'`
ou `'serial'`. `zonesPerSecond` impose un coût supplémentaire d’installation
par zone. L’occupation estimée du disque, le nombre d’items par zone, la forme
de la demande par préfixe et la durée du boot restent des paramètres de scénario.
La rampe idéale génère 11 760 000 demandes contre 11 825 088 mesurées.
Les rejeux horodatés utilisent les observations réelles. Le modèle en boucle
fermée sert à rechercher des contre-exemples. Il n’est pas qualifié pour prédire
une latence client, la durée d’un gel ou le nombre exact de machines.

Vérifier avec `python3 -m unittest test_aws_replay` et `node --test *.test.mjs`.
