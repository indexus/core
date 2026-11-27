# Qualification de 9f0f09c — 26 novembre 2026

> Mise à jour : la qualification AWS autorisée ensuite est décrite dans le
> [rapport AWS](qualification-9f0f09c-aws-2026-11-26.md). Le présent document
> conserve les observations locales antérieures à cet accord de budget.

- **Correctif fusionné par avance rapide et poussé sur main ; qualification complète encore inachevée.**
- Formatage, compilation, vet, suites normales/H3, huit paquets avec race detector et tests Python passent.
- Un passage dynamique à 64 nœuds termine avec 8 278 ACK, sans perte, doublon ni gel restant ; aucune faute injectée dans ce passage.
- La comparaison de performance est suspendue : activité de swap pendant le labo et environ 5,9 % de disque libre, sous le seuil de placement de 35 %.
- Aucun nouveau code de production ni changement de claude/next ; aucune ressource AWS lancée pour cette passe.

## Source figée et changements

Source : `9f0f09c111e9301dea5fb9c8e3345250eb3b3b8d` ; arbre :
`f09066d7842567776cacf89ef488b0e928066ba2`. Qualification dans un worktree
externe détaché. Les fichiers non suivis du checkout principal sont conservés.
Ce rapport est ajouté ensuite, sans changer la source mesurée.

Le correctif de Claude ferme les trois défauts de câblage du
[rapport précédent](qualification-9ec4688-2026-11-26.md) :

| Défaut | Correction lue dans le code | Vérification réalisée |
| --- | --- | --- |
| Hook facts absent des hôtes | Téléchargement récursif du dossier elastic | Test statique de déploiement ; pas encore de téléchargement observé sur EC2 |
| Agent issu du loader historique | Binaire agent construit depuis la version testée, indépendamment du générateur | Test statique et tests Go ; coexistence réelle sur EC2 non qualifiée |
| H3 sans profil CPU | Tag diagnostic, port local par slot et relais `/profile` de l'agent | Compilation normale/diagnostic et tests du relais contre un faux nœud ; chaîne H3 réelle non qualifiée |

`CPU_PROFILE=1` expose le profil mais ne l'enregistre pas : les scripts standards
ne demandent pas `/profile`. La capture des receveurs doit être organisée
explicitement depuis une machine du réseau privé. L'existence du handler ne
constitue pas une preuve de collecte.

Les onze commits précédents sont décrits dans le rapport précédent et dans
[duplicate-work-audit-20261126.md](duplicate-work-audit-20261126.md).
Ils évitent des parcours, lectures, encodages et empreintes en double, découpent
les grosses fonctions et ajoutent le placement par ressources et le monitoring
avant join. Leur solde Go de production reste **+1 252 lignes**, hors tests,
entre a937b18 et 9ec4688. La simplification concerne le travail effectué et la
structure ; elle ne correspond pas à une diminution globale du code.

## Étape 1 — résultats locaux mesurés

Les contrôles sont séquentiels, avec `-p 1` pour les paquets Go. Les tests
unitaires gardent la vérification exhaustive d'usage par défaut.

| Contrôle | Résultat |
| --- | --- |
| `gofmt -l .` | PASS, sortie vide |
| `python3 ops/experiment/aws/test_deployment.py` | PASS |
| `go build ./...` et `go vet ./...` | PASS |
| H3 : build normal et `-tags diagnostic` | PASS |
| `go test -p 1 ./...` | PASS, 195,09 s |
| Module quic : `go test -p 1 ./...` | PASS, 6,65 s |
| `-race` : mesh, storage, summary, zonelog, autoscale, adapter/provisioner, app/node, cmd/experiment | PASS |
| `python3 ops/experiment/elastic/test_spawn.py` | PASS |

Le manifeste contient chaque commande, sa durée, son code de sortie et son
journal. Ces succès ne qualifient pas à eux seuls le déploiement réel du
provisioner et des agents.

## Étape 2 — un passage 64 nœuds terminé ; campagne interrompue

Configuration : HTTP direct sur loopback, 64 services/stores, 32 hôtes simulés,
8 192 zones initiales, budget store 32 Mio, charge globale fixe de 40 écritures/s,
GOMAXPROCS=8, fsync et maintenance actifs, joins pendant les écritures, sans
fautes. Tous les nœuds partagent le CPU, le SSD et le ramasse-miettes du portable.
Le mode de comptabilité incrémentale du benchmark est activé explicitement ;
les tests de l'étape 1 ont gardé leurs contrôles exhaustifs.

| Mesure observée, passage 1 | Valeur |
| --- | ---: |
| ACK / échecs / shed | 8 278 / 0 / 0 |
| Records observés, avec les 8 192 initiaux | 16 470 |
| ACK manquants / dupliqués | 0 / 0 |
| ACK absents ou dupliqués parmi les détenteurs écrivables | 0 / 0 |
| Gels restants à l'audit | 0 |
| Durée de charge indiquée par l'audit | 208,19 s |
| Durée totale du test | 222,14 s |
| Latence ACK p50 / p95 / p99 | 47,279 / 183,664 / 276,871 ms |
| Latence depuis l'arrivée prévue p99 | 287,997 ms |
| p99 ACK des phases initiales, 1 598 écritures | 335,302 ms |
| Profil d'allocations cumulées, préparation et audit compris | Environ 44,2 Gio échantillonnés |

Les percentiles locaux sont calculés à partir des traces individuelles,
contrairement aux bornes de classes log2 de certains tableaux AWS. La durée
de charge/audit ci-dessus n'est pas une durée de gel et ne doit pas être
renommée durée exacte de fermeture des joins.

### Pourquoi cette performance ne qualifie pas un gain

Sur la fenêtre analysée, l'hôte conserve au moins 45,51 % de CPU inactif et
36 % de mémoire libre selon memory_pressure. Cela ne suffit pas : les
compteurs globaux de swap augmentent. Entre le premier et le dernier
échantillon hôte du passage : +1 585 pages entrantes, +15 860 pages sortantes
et +2 719 pageouts ; page physique de 16 384 octets. Ces compteurs concernent
l'hôte entier, pas uniquement Indexus. Leur origine n'est pas attribuée au
protocole. La sonde FULLFSYNC a un p99 de 33,485 ms dans la fenêtre analysée.

Le premier passage a terminé avant la prise en compte de l'interruption ;
l'observateur avait automatiquement démarré le deuxième. Celui-ci a été
terminé sans audit ni résultat PASS. Le passage avec fautes n'a pas été lancé.
Tous les processus possédés par cette campagne sont arrêtés.

| Comparaison demandée | Statut honnête |
| --- | --- |
| 9f0f09c, deux répétitions sans fautes | Une complète, seconde interrompue ; performance non qualifiée |
| 9f0f09c, une répétition avec fautes | Non exécutée |
| 2362b35 et 1afbd3d sur le même environnement sain | Non rejoués dans cette passe |
| Commits par catégorie, avant/après comparable | Comparaison non produite |

Les anciennes mesures restent historiques. Le passage actuel ne permet pas
un pourcentage de gain ou de régression face à ces versions, ni une conclusion
de p99 indépendant de la taille du réseau. L'audit de fin est réussi mais ne
prouve pas, à lui seul, toutes les lectures exactes à chaque instant du join.

## Étapes 3/4 — placement réel et garanties temporelles non qualifiés

L'APFS ne dispose que d'environ 5,9 % d'espace libre ; le placement demande
35 %. Aucun volume de données indépendant ni contexte Docker distant n'a été
trouvé. Docker local partage le même disque physique et ne résout pas cette
précondition. La marge RAM Linux n'est pas non plus établie sur macOS.

Les cas normaux local/existing/new ne sont donc pas déclarés verts à partir de
faits artificiels ou du fallback de plafond. Aucun cluster réel n'a été lancé.
Les assertions monitoring avant fin de join, health 503 avant activation,
profil H3 relayé réel, lectures exactes et écrivain unique pendant join restent
à vérifier de bout en bout.

Choix recommandé après le retour de l'utilisateur : une qualification courte
sur des machines AWS avec faits réels et capacité disponible, avant le test de
charge. Elle doit aussi reprendre le labo 64 nœuds dans un environnement sans
swap. Les préparatifs locaux et les commandes sont conservés dans les outputs ;
ils ne sont pas présentés comme des tests exécutés.

## AWS — préparé, pas lancé

Le préflight en lecture seule du 26 novembre à 11:55 UTC indique zéro expérience
active, quota 512 vCPU, autre usage 2 vCPU, prix relevés 0,398 USD/h pour un
hôte i4i.xlarge et 0,808 USD/h pour le loader c6i.4xlarge. Il faudra le rafraîchir
avant lancement. Aucune ressource payante n'a été créée par cette passe.

Cible de charge conservée : DIRECT/H3, 2k → 4k → 8k → 16k/s, deux slots par
hôte, loader figé à 2362b35, jusqu'à 32 hôtes/64 vnodes, TTL 45 minutes. Le
budget doit être confirmé avant tout lancement. Le benchmark complet reste
conditionné à la qualification réelle précédente et à une collecte effective
des stacks ET profils CPU des receveurs.

## Traces et reprise

Dossier de travail externe : `outputs/qualification-9f0f09c-20261126/`.
Il contient `local-qualification.json`, les journaux de l'étape 1, `lab64/`,
`analysis/comparison.json`, `alloc-top.txt`, `resource-stop.json`, le runbook
`local-cluster/` et le préflight/plan de capture dans `aws/`.

Aucun nouveau défaut de code n'est déclaré à partir du swap. Aucun correctif
ni assouplissement d'assertion n'a été ajouté. La reprise demande un environnement
adapté et un budget confirmé, puis les étapes restantes dans l'ordre ; les
anciens défauts de câblage ne suffisent plus à expliquer un futur décrochage.
