# Qualification locale après correction de l’audit — 26 novembre 2026

> Suite : le [run AWS](aws-settled-direct-2026-11-26.md) ne qualifie pas 16k/s.
> Le [passage local sur `36e2b31`](claude-copies-2026-11-26.md) échoue sur un
> total exact 503. Le succès ci-dessous reste celui de ce seul passage sur `0b4ddb4`.

Le passage unique sur `0b4ddb43100b25de39d917f68ce7fe4a8c3cdd25` réussit :
**17 999 écritures demandées et acquittées, toutes relues, et les totaux exacts
des 17 collections correspondent aux ACK**. Aucun double détenteur n’est
observé par l’audit. Celui-ci attend réellement la fin des mouvements avant de
contrôler les totaux : 24 sondages, soit 5 841 ms, puis zéro transfert ouvert,
gelé ou join en cours. Ce délai est une attente d’audit, pas un gel des écritures.

Cette exécution qualifie le correctif de mesure décrit dans le
[passage précédent](demand-qualification-2026-11-26.md). Aucun changement du
protocole ni des seuils n’a été ajouté entre les deux passages. Les assertions
ne sont pas affaiblies et les totaux ne sont pas relus en boucle pour obtenir
un succès.

## Conditions

- Linux arm64, Docker Desktop : huit CPU visibles et environ 8,32 Go de mémoire.
- Un contrôleur et deux agents réels, avec les hooks du provisioner. Le donneur
  démarre avec cinq vnodes ; la réserve offre cinq emplacements vides.
- Les deux pools partagent la même VM, son CPU et son disque. Il ne s’agit pas
  d’une qualification sur deux machines physiques indépendantes.
- HTTP direct, 400 écritures/s pendant 45 s, seed 19. La moitié des écritures
  vise le préfixe `Hot`, dans des zones feuilles distinctes ; le reste est
  réparti entre seize collections. Ce n’est pas une seule zone chaude.
- Signal de demande : 150 écritures/s/vnode, paramètre du banc et non une
  recommandation de capacité en production. Cible CPU 60 %, deux jetons de
  copie, `GOMAXPROCS=2`, `GOMEMLIMIT=512MiB` par processus.
- Un seul essai ; aucune campagne AWS ni relance automatique.

## Le trafic commence avant la fin du join

Les temps du tableau sont relatifs au début de la capture. L’observation est
échantillonnée, environ toutes les 0,5 s. Le délai interne de première écriture
mesure une **application durable**, pas l’arrivée de l’ACK chez le client.

| Receveur sur la réserve | Première observation | Première application observée | Application après démarrage | Join terminé, observé | Applications finales |
| --- | ---: | ---: | ---: | ---: | ---: |
| Slot 0 | 13,08 s | 13,62 s | 303 ms | 17,22 s | 3 654 |
| Slot 1 | 28,69 s | 29,24 s | 363 ms | 33,53 s | 3 047 |
| Slot 2 | 44,95 s | aucune | — | 58,08 s | 0 |

Le provisioner décide un placement `new` vers la réserve, puis deux placements
`local` sur cette même réserve. Les deux premiers receveurs traitent donc des
écritures pendant leur join. Le troisième arrive à la fin de la charge ; il
termine son transfert mais son soulagement du trafic n’est pas mesuré.

Le `final_reserve` de `result.json` est une capture antérieure à la fin du
dernier join : il indique encore un receveur en cours. La dernière télémétrie
et l’audit ultérieur confirment sa clôture. Ne pas confondre ces instants.

## Latences et ressources

Bornes supérieures des cases d’histogramme, en millisecondes. Les fenêtres
classent les points publiés par leur temps de publication, pas chaque requête
par son instant de départ. La dernière fraction non publiée est absente des
histogrammes : 17 596 observations pour 17 999 ACK finaux.

| Fenêtre de publication | Observations écriture | p50 | p95 | p99 | p99 depuis l’arrivée prévue |
| --- | ---: | ---: | ---: | ---: | ---: |
| 0–12 s | 4 396 | 8,192 | 8,192 | 16,384 | 32,768 |
| 12–28 s | 6 400 | 8,192 | 32,768 | 262,144 | 262,144 |
| 28–45 s | 6 800 | 8,192 | 16,384 | 32,768 | 65,536 |
| Tous les points publiés | 17 596 | 8,192 | 16,384 | 131,072 | 131,072 |

Les 353 lectures publiées donnent p50 2,048 ms, p95 8,192 ms et p99 65,536 ms.
Le pic de latence du premier transfert reste visible : ce succès ne signifie
pas que la latence pendant les joins est résolue.

Pendant la charge, le maximum CPU observé de la VM est **45,7 %**. Après 45 s,
pendant le travail restant et l’audit, il monte à **66,4 %**. La cible de 60 %
n’est donc pas un plafond garanti. La mémoire libre reste au-dessus de 72,1 %
et le disque libre au-dessus de 41,87 % sur toute la capture. Ces ressources
sont partagées avec le contrôleur et les autres services de la VM ; les valeurs
des deux agents ne doivent pas être additionnées. Cette capture ne suffit pas
à attribuer le pic CPU à un composant précis.

## Conservation et limite des compteurs consultatifs

L’audit relit 17 999 clés, sans manque ni erreur. Il vérifie les totaux exacts
des 17 collections et ne trouve aucune zone détenue deux fois. Il répond sur
les huit vnodes. Aucun refus, abandon ou travail inachevé n’est déclaré par le
générateur de charge.

En revanche, la somme des compteurs consultatifs `items_held` vaut **22 474**.
Elle surestime le total exact de 4 475. Les compteurs locaux ne constituent
donc pas une preuve de cardinalité après transfert. L’écart est conservé dans
les traces ; sa cause précise et sa convergence ne sont pas établies par cet
essai. Le dashboard doit être lu avec cette distinction. Aucune donnée n’a été
modifiée pour aligner ces compteurs.

Le contrôle de double propriété porte sur l’état observé à l’audit ; ce run
seul ne prouve pas l’écrivain unique pour toutes les interleavings. Il ne
remplace ni les modèles de sûreté ni les tests avec pannes.

## Reproduction et traces

Depuis le checkout propre à la révision indiquée, construire les quatre
binaires Linux arm64 `node`, `issuer`, `experiment`, `provisioner`, puis :

```sh
python3 ops/experiment/multiprocess/placement.py \
  --bin /absolute/path/bin --out /absolute/path/run \
  --image indexus/quickstart:review-arm64-1ce3f58eb8d8 --trigger demand
python3 ops/experiment/multiprocess/replay.py \
  /absolute/path/run /absolute/path/dashboard
```

[Replay commun au dashboard](settled-demand-20261126/live.html),
[mesures structurées](settled-demand-20261126/summary.json),
[audit](settled-demand-20261126/audit.json),
[empreintes des traces et binaires](settled-demand-20261126/manifest.json).
Les douze frames du replay, les groupes de machines, les destinations de
transfert et la syntaxe JavaScript ont été vérifiés. Le rendu visuel n’a pas
été revérifié dans le navigateur. Aucun store ni secret n’est publié.

Les conteneurs et le réseau créés pour cet essai sont supprimés. Les services
préexistants restent en place. L’étape locale demandée est terminée. Une
qualification sur machines distinctes, H3 et une charge plus longue restent
nécessaires avant de conclure sur AWS, sur 64 vnodes ou sur la linéarité.
