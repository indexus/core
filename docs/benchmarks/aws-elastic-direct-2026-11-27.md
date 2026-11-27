# AWS direct — arrêt à 4k/s et écart client/serveur, 27 novembre 2026

**Ce passage ne qualifie ni 8k/s ni 16k/s.** Il est arrêté manuellement au
palier 4k/s après accumulation de la file du générateur et dégradation de sa
latence. Les mesures serveur et celles du client divergent fortement : il ne
faut pas lire les secondes du graphique client comme des secondes de gel du
protocole. Toutes les ressources de ce run sont supprimées et vérifiées.

## Source, intégration et configuration

Run `aws-elastic-direct-1127-042221`, source, agent et loader figés sur
`124ccdd`. Le travail Claude `052ca47` est déjà repris par `b1a69b7`, complété
par `43a546a` : placement distant, réutilisation de capacité distante avec
observations connues, expiration des comptages et ordre de tri cohérent.
Il n'est pas fusionné une seconde fois.

- H3 entre pairs, client TCP/TLS en routage direct.
- Quatre `i4i.xlarge` initiales, cinq slots chacune, **seize vnodes initiaux**.
- Un loader `c6i.4xlarge`, 4 096 workers ; plafond 32 hôtes, budget 8 USD,
  TTL 30 minutes. Prix et quota résolus par les API AWS avant provisionnement.
- Programme prévu 2k → 4k → 8k → 16k/s, plateaux 120 s, rampes 60 s,
  tail 180 s. Un seul passage, aucune relance automatique.
- Demande 200 écritures/s/vnode, seuil consultatif 500 000 items,
  `WRITE_CAP=0`, signaux ressources actifs et réserve 15 %. Seuil CPU inchangé.
- `SAMPLE=0` demandait la relecture de tous les ACK en fin de scénario.
  Cet audit de conservation n'est **pas achevé** après l'arrêt anticipé.

Ce n'est pas un A/B de code : le [run précédent](aws-settled-direct-2026-11-26.md)
démarrait quatre vnodes avec une demande de 500/s/vnode. La distribution reste
uniforme sur 256 collections, emplacements aléatoires de longueur 16, avec
une collection supplémentaire pour les sondes. Node, agent, loader et plan
proviennent du même checkout propre. Builds, formatage, sept tests de
déploiement et 21 tests de hooks passent ; la qualification Go/race de ce
code est détaillée dans [la passe locale](elastic-anticipation-2026-11-26.md).

## Mesuré pendant la charge

Bornes supérieures des buckets logarithmiques, en millisecondes, agrégées
sur les publications des fenêtres ; pas des percentiles exacts.

| Fenêtre | Demande | Client p50 | p95 | p99 | p99 depuis arrivée prévue |
| --- | --- | ---: | ---: | ---: | ---: |
| 0–120 s | 2k/s | 4,096 | 4,096 | 8,192 | 16,384 |
| 120–180 s | rampe 2k → 4k | 4,096 | 8,192 | 16,384 | 16,384 |
| 180–210 s | 4k/s | 4,096 | 8,192 | 16,384 | 32,768 |
| 210–240 s | 4k/s, joins | 8,192 | 1 048,576 | 1 048,576 | 1 048,576 |
| 240–297 s | 4k/s | 2 097,152 | 2 097,152 | 2 097,152 | 8 388,608 |

À t257, le dashboard publie 4 000/s demandées, 3 368/s acquittées, p99
2 097 ms, 4 635 en file, 34 vnodes et 26 hôtes. La dernière publication du
loader, à t297, donne 3 269 ACK/s, 4 096 en vol et 14 124 en file. Le maximum
de file observé est 16 384. La flotte atteint 35 vnodes. Des machines restent
disponibles ; l'offre physique ne suffit donc pas à expliquer cette file.
Un pic CPU hôte à 100 % est observé pendant les joins ; le loader utilise
ensuite environ 3,3 de ses 16 vCPU, ce qui ne prouve pas l'absence d'un verrou
sérialisant une partie de son travail.

Le résumé ordinaire se réconcilie exactement :

| Catégorie | Nombre |
| --- | ---: |
| offertes | 890 490 |
| acquittées | 868 615 |
| shed | 7 883 |
| exhausted | 4 106 |
| queued_unfinished | 9 886 |

Les requêtes sans ACK ont une issue indéterminée. Le ledger conservé contient
869 458 lignes : il inclut les sondes, absentes du compteur ordinaire. Il ne
constitue pas une preuve de relecture de ces écritures. Une première compression
lancée pendant le drain a signalé un fichier en croissance ; une seconde
collecte attend la fin du processus puis publie le gzip atomiquement.

## La latence serveur ne suit pas les secondes du client

Différences des histogrammes cumulatifs de chaque nœud entre les bornes de
fenêtre, à partir de l'observateur réel :

| Fenêtre | Requêtes observées | Handler serveur p50 | p95 | p99 |
| --- | ---: | ---: | ---: | ---: |
| 180–210 s | 119 872 | 4,096 | 8,192 | 16,384 |
| 210–240 s | 120 099 | 4,096 | 16,384 | 32,768 |
| 240–297 s | 211 764 | 4,096 | 8,192 | 32,768 |

Le handler inclut contrôle de scope, décodage et réponse. Les fenêtres des
deux observateurs ne sont pas atomiquement synchronisées, mais cet écart
majeur mérite d'isoler le client et le transport avant d'accuser le stockage.

Sur 5 823 cutovers réussis du snapshot conservé :

- `cutover_call_us_log2` : p50 32,768 ms, p95/p99 65,536 ms, dernier bucket
  occupé 131,072 ms. Ce n'est pas la durée du join complet.
- `freeze_return_to_install_ack_us_log2` : p99 32,768 ms ; cette mesure
  commence **après le retour de la fence**, elle n'est pas le gel complet.
- `install_to_first_write_us_log2` n'a aucun échantillon : ne pas en déduire
  un délai nul ou une activation mesurée. Il manque une mesure exploitable
  pour les premiers droits natifs et les écritures dans l'espace vide.

Après arrêt, les transferts observés se ferment et le CPU revient vers 16 %.
Cela ne démontre pas un deadlock permanent. La sentinelle n'a pas provoqué
l'arrêt : ses règles ne surveillent pas directement la file ou le p99 client,
et son seuil de stagnation est plus bas que le déficit constaté ici.

## Lu dans le code, puis reproduit localement

Le générateur apprend une route **après** avoir reçu un HTTP 201 et **avant**
de terminer sa mesure de latence (`cmd/experiment/main.go`). `routes.learn`
reconstruit un complément de régions pour chaque ACK sous un `RWMutex`
commun à tous les workers (`cmd/experiment/routes.go`).

Une indication ponctuelle est légitime : `Store.WriteRegionHint` la renvoie
notamment dans une région réservée pour transfert. Le client possède déjà
un cache exact `learnZone`, mais il soustrait aussi chaque point de ses
régions générales. Le nombre d'entrées est plafonné ; le travail et les
allocations par ACK ne sont pas pour autant petits.

Probe diagnostic, 1 000 opérations par cas, Apple M1 Pro, un passage,
aucun réseau. Le cas fragmenté répète un même point après amorçage :

| Amorçage | Régions résultantes | Temps/apprentissage | Octets/op | Allocations/op |
| --- | ---: | ---: | ---: | ---: |
| région entière, ACK identique | 1 | 215 ns | 128 | 3 |
| région entière puis 16 points | 1 575 | 50,9 µs | 119 828 | 1 576 |
| région entière puis 64 points | 4 096 | 119,7 µs | 303 114 | 4 098 |

À 4k ACK/s dans ce dernier état, le calcul extrapole environ 1,21 Go/s
alloués. **Ce n'est pas une mesure des allocations du loader AWS.** Le nombre
de régions de son cache et son attente de verrou n'étaient pas exportés.
La taille réelle du cache au moment du décrochage reste inconnue : le probe
reproduit un coût de fonction dans un état construit, pas le run complet.

Hypothèse forte : cette sérialisation et ce renouvellement d'objets contribuent
au plafond du générateur, malgré une moyenne CPU machine faible. À confirmer
par profil CPU/mutex du loader et chronométrage séparé réponse HTTP /
apprentissage / écriture du ledger. Les profils de nœuds ne mesurent pas ce
travail client.

## Deuxième piste : admission encore tardive côté donneur

`handoffLocked` lit l'état et appelle `transferOffer` **avant** `copySlot`.
L'exécution des copies est bornée, mais pas nécessairement leur préparation
et leurs décisions durables en attente. Dans le snapshot d'arrêt, sept
opérations tournantes sont en `copy-slot` avec zéro tentative commencée,
et une en `stage-region`.

Cela justifie de mesurer et borner la préparation avant d'ouvrir des
opérations nouvelles, en laissant toujours passer la reprise d'une fence
existante. Ce n'est pas encore la preuve que cette préparation a causé les
secondes du graphique client. Les deux pistes doivent rester séparées.

Deux profils de dix secondes sont conservés, prélevés vers t214 : donneur
slot 3 et son receveur, phase annoncée `install`. Ils ne couvrent ni les
quatre processus du donneur ni nécessairement son pic CPU. Ils totalisent
respectivement 1,53 et 0,38 CPU-seconde ; ne pas les présenter comme un profil
complet de l'hôte saturé.

## Suite proposée, sans nouvelle campagne automatique

1. Instrumenter le client : nombre de régions, attente/tenue de son verrou,
   durée HTTP distincte de l'apprentissage et du ledger ; profiler aussi le
   loader, pas seulement les nœuds.
2. Éviter que les preuves ponctuelles fragmentent le cache régional : le cache
   exact sait déjà les retenir. Rendre l'apprentissage identique idempotent.
   Conserver les compléments et les cas split/fold ; ne pas élargir un point
   en autorité régionale.
3. Rejouer ce chemin avec des milliers de points, concurrence et vrais clients
   directs. Le labo mesh à 64 nœuds ne traverse pas le cache du loader ; le
   passage à huit processus/400 écritures/s ne couvrait pas ce régime.
4. Séparer l'admission d'une opération nouvelle de sa préparation coûteuse,
   après mesure, sans temporiser une reprise déjà gelée.
5. Ensuite seulement refaire AWS, sans relever les seuils de ressources pour
   masquer le coût. Le placement seul n'est pas déclaré suffisant.

Les corrections de production n'ont pas été changées pendant ce run, ni par
le probe. Son fichier de test temporaire est retiré du checkout ; sa source
est conservée comme [artefact diagnostic](aws-elastic-direct-20261127/incident-routes-probe.go.txt).

## Preuves et nettoyage

[Résumé et configuration](aws-elastic-direct-20261127/summary.json),
[fenêtres client](aws-elastic-direct-20261127/latency-windows.json),
[fenêtres serveur](aws-elastic-direct-20261127/server-latency-windows.json),
[cutovers](aws-elastic-direct-20261127/transfer-summary.json),
[probe](aws-elastic-direct-20261127/routes-point-probe-final.log),
[profil donneur](aws-elastic-direct-20261127/donor-top.txt),
[profil receveur](aws-elastic-direct-20261127/receiver-top.txt).
Les logs bruts, profils pprof et ledger compressé restent dans les outputs
locaux du run ; les credentials et clés d'issuer ne sont pas publiés.

Vérifications après teardown : aucune instance de ce run en attente, active,
arrêtée ou en cours d'arrêt ; VPC, security group, launch template, rôle IAM,
instance profile et bucket supprimés. Les ressources hors de ce run sont
préservées. Aucun second essai n'est lancé.
