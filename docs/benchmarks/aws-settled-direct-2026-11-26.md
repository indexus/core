# AWS direct : arrêt pendant la montée — 26 novembre 2026

**Ce passage ne qualifie pas 16k/s.** La charge est arrêtée après une
saturation persistante pendant le palier 4k/s, avec un p99 de fenêtre jusqu’à
524 ms. L’arrêt effectif arrive au début de la rampe suivante, vers 4,8k/s.
La sentinelle signale ensuite un `handoff_loop` à trois tentatives pendant la
phase de réduction. L’audit final est incomplet ; aucun succès de conservation
globale n’est déclaré. Toutes les ressources AWS de cette campagne sont supprimées.

## Source et configuration

Run `aws-settled-direct-1126-2031`, source, agent, loader et infrastructure
figés sur **`c2f226a`**. Les trois commits ultérieurs de Claude ne font pas
partie de ce run ; leur [qualification distincte](claude-copies-2026-11-26.md)
échoue sur un total exact indisponible.

- Quatre `i4i.xlarge` au départ, cinq emplacements de vnode par hôte, quatre
  vnodes initiaux. Plafond de 32 hôtes, enveloppe de 8 USD, TTL 30 minutes.
- Un loader `c6i.4xlarge`, 4 096 workers. H3 entre pairs, routage direct.
- Programme prévu : 2k → 4k → 8k → 16k/s, plateaux 120 s, rampes 60 s,
  tail 180 s, un seul passage. Aucun redémarrage automatique de campagne.
- Demande 500 écritures/s/vnode, seuil 500 000 items, signaux de ressources
  actifs, cible CPU 60 %, réserve 15 %, profils CPU activés.
- Charge uniforme sur 256 collections, emplacements aléatoires de longueur 16.
  Les feuilles ont essentiellement un record chacune. Une 257e collection sert
  aux sondes de fraîcheur ; ce scénario n’est pas une seule zone chaude.

Les précontrôles passent sur cet arbre : formatage, builds core et quic,
suite Go complète, `-race` sur storage/autoscale/provisioner/node/mesh,
21 tests de hooks et six tests de déploiement. Le mode `steps` ne démarre pas
de sentinelle : un observateur externe utilise le binaire actuel et la liste
dynamique des agents, sans modifier les nœuds.

## Mesuré pendant les écritures

Quantiles agrégés des cases logarithmiques publiées, en ms : ce sont des
bornes supérieures. Les fenêtres classent les publications, pas les requêtes
individuelles par leur départ.

| Fenêtre | Charge | p50 | p95 | p99 | p99 intentionnel |
| --- | --- | ---: | ---: | ---: | ---: |
| 0–120 s | 2k/s | 4,096 | 4,096 | 8,192 | 16,384 |
| 120–180 s | rampe 2k → 4k | 4,096 | 8,192 | 8,192 | 16,384 |
| 180–210 s | début du plateau 4k | 4,096 | 16,384 | 32,768 | 32,768 |
| 210–300 s | plateau 4k, nouveaux joins | 8,192 | 65,536 | 131,072 | 131,072 |
| 300–315 s | début de rampe puis interruption | 8,192 | 65,536 | 262,144 | 262,144 |

Le dashboard, sur ses fenêtres plus courtes, atteint **524,288 ms de p99**.
À t297 : environ 4k/s acquittées, 20 vnodes, neuf transferts ouverts dont six
gelés, maximum CPU hôte 92,1 %. La file et les abandons du générateur restent
nuls. Les relais restent faibles, au plus 0,6 % sur les points publiés.

Le résumé du générateur compte **960 825 demandes ordinaires, 960 761 ACK et
64 épuisements lors de l’interruption**. Le ledger contient **961 826 clés
distinctes acquittées**, car il inclut aussi 1 065 sondes, absentes des compteurs
de charge ordinaires. Les 64 demandes ordinaires sans ACK ont une issue
indéterminée ; l’égalité « nombre durable = nombre d’ACK » n’est pas une
condition valable sans examiner ces écritures.

La flotte monte à 15 hôtes de données et 22 vnodes. À la fin de charge, sur les
observations entre t280 et t315, les deux vnodes les plus chargés d’un même hôte
appliquent environ 421 et 378 écritures/s. Six des 21 vnodes observés dans cette
fenêtre sont sous 100/s ; certains viennent de démarrer, donc leurs fenêtres
d’observation sont plus courtes. Voir les durées dans la trace structurée.
Ajouter des processus ne soulage pas instantanément les détenteurs installés.

## Profils et interprétation

Une paire de profils de dix secondes est capturée tôt, puis une deuxième
pendant la pression à 4k/s. Dans cette dernière :

- Donneur échantillonné : 20,43 secondes CPU en 10,01 secondes, soit environ
  deux cœurs consommés par ce processus, sur quatre vCPU par hôte.
- Coût cumulé `Service.handoff` : 22,2 % des échantillons du donneur.
- `Tree.boundaryKeys` : 15,0 % ; `FlushPendingEdges` : 17,8 % ;
  `PollRemoteHosts/Reconcile` : 16,4 %. Ces piles peuvent se recouvrir :
  leurs pourcentages ne s’additionnent pas.
- Receveur échantillonné : 4,81 secondes CPU en dix secondes ;
  `reservationDelta` : 5,0 %, `checkCapacity` : 6,2 %. Un seul profil de paire
  ne représente pas tous les receveurs ni leurs pointes.
- Sur le donneur du profil, les compteurs incluent 37 inventaires trop grands
  pour être conservés dans le cache (`export_keys_oversized`). Cela indique
  encore du travail sur les frontières ; cela ne prouve pas une nouvelle cause
  quadratique.

**Lu dans le code :** les jetons de copie bornent une concurrence, pas un
pourcentage CPU. Une admission du receveur ne réserve pas tout le surcroît de
travail du donneur. Le budget estimé de réception de 1,75 cœur vaut 44 % d’un
hôte à quatre vCPU ; il peut limiter le placement tandis que les donneurs
continuent leurs copies et résumés. Les décisions et les métriques retardées
ne constituent pas une limite cgroup.

**Hypothèse à tester :** commencer le renfort plus tôt et réguler le débit de
copie à la source comme à la destination, plutôt que multiplier les réceptions
sans budget CPU partagé avec les écritures et résumés. Un seuil de demande de
200/s pourrait anticiper, mais augmenterait les vnodes et les mouvements :
aucun résultat de cette campagne ne le qualifie. Réduire encore les zones ne
résout pas le coût fixe lorsque les feuilles contiennent déjà un seul record.

Après arrêt, les replis à froid font redescendre la flotte de 22 à six vnodes,
avec encore des mouvements à la dernière capture. Leur ordonnancement doit
aussi être mesuré. Les audits lancés pendant cette période ajoutent de la
charge : ces données après arrêt ne constituent pas une mesure de coût à vide.

## Audit interrompu et limites

Un audit de drainage commence par l’échantillon standard de 20 000 clés, puis
contrôle 257 totaux et signale **55 écarts numériques, aucun total indisponible**.
Il n’écrit pas de rapport final avant interruption : les valeurs détaillées et
la conservation exhaustive ne sont donc pas certifiées. Les 64 écritures sans
ACK peuvent contribuer aux écarts ; leur responsabilité exacte n’est pas établie.

Le chemin supplémentaire de relecture des collections en écart est séquentiel.
Il réessaie également les endpoints retirés. Son annulation ne coupe pas ses
boucles en cours : des audits supposés annulés continuent jusqu’à leur arrêt
explicite. Cet incident de mesure est conservé, pas assimilé à une perte de données.

Un lecteur diagnostique séparé est compilé après l’arrêt de charge : refus TCP
immédiatement rendu au mécanisme de repli, relectures supplémentaires bornées
à 32 workers, pas de deuxième passage séquentiel si toutes les clés ont déjà
été relues. Ses tests, dont la conservation d’un verdict d’échec lorsque les
totaux divergent, passent sous `-race`. Ce prototype **n’est pas intégré à main**.
Ses lectures restent partielles ; elles ne remplacent pas le rapport manquant.
Les périodes d’audits se recouvrent avant l’arrêt forcé des anciens processus.
Les profils et mesures de charge cités plus haut précèdent tous ces changements.

À **20:53:23 UTC**, la sentinelle signale
`handoff_loop https://10.77.14.255:19100 3 attempts`, pendant la réduction.
La capture est terminée, les lecteurs arrêtés et les ressources supprimées.
Aucun seuil n’a été relevé et aucun nouvel essai AWS n’a suivi.

## Ressources et prochaine qualification

L’API AWS confirme pour `i4i.xlarge` quatre vCPU, 32 Gio et 937 Go NVMe.
La RAM et le disque ne sont pas les contraintes de ce run. Pour réduire leur
surdimensionnement, candidats disponibles à Paris :

| Instance | vCPU | RAM | NVMe | USD/h Linux à la demande |
| --- | ---: | ---: | ---: | ---: |
| i4i.xlarge | 4 | 32 Gio | 937 Go | 0,398 |
| c6id.xlarge | 4 | 8 Gio | 237 Go | 0,2415 |
| c7gd.xlarge | 4 | 8 Gio | 237 Go | 0,2174 |
| c6id.large | 2 | 4 Gio | 118 Go | 0,12075 |
| c7gd.large | 2 | 4 Gio | 118 Go | 0,1087 |
| c7gd.medium | 1 | 2 Gio | 59 Go | 0,0543 |

Tarifs lus via AWS Pricing le 26 novembre, hors frais annexes. Les C7gd sont ARM
et nécessitent un harnais adapté ; leurs vCPU sont des cœurs, contre deux threads
par cœur pour les Intel cités. Sur deux vCPU, une réservation de réception de
1,75 cœur dépasse déjà la cible 60 % : mesurer et réguler ce coût est un
prérequis, sans relever la cible. Garder cinq emplacements n’impose jamais cinq
processus actifs. Références : [C6id](https://aws.amazon.com/ec2/instance-types/c6i/),
[C7gd](https://aws.amazon.com/ec2/instance-types/c7g/).

Avant un autre AWS : résoudre le 503 paginé reproduit localement, mesurer le
CPU du donneur et du receveur avec plusieurs copies, et rendre l’audit
interruptible et borné de bout en bout. Une simple hausse de flotte ne qualifie
pas ces mécanismes.

[Dashboard capturé](aws-settled-direct-20261126/live.html),
[mesures](aws-settled-direct-20261126/summary.json),
[distribution](aws-settled-direct-20261126/distribution-at-stop.json),
[traces et empreintes](aws-settled-direct-20261126/manifest.json),
[preuve du nettoyage](aws-settled-direct-20261126/cleanup-verification.json).
Les ACK synthétiques complets et traces volumineuses restent dans les outputs
locaux ; aucun store ni secret n’est publié. Aucun résultat de facturation
définitif n’est déduit de l’enveloppe de planification.
