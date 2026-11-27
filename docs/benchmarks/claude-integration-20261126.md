# Intégration du patch Claude et comparaison AWS — 26 novembre 2026

Le code intégré est `1afbd3d`, fusionné en avance rapide et poussé sur `main`.
La référence AWS est `2362b35`. Les expériences utilisent toutes le loader
`2362b35`, avec la même instrumentation des demandes encore en vol après charge.
Aucun binaire n'est remplacé pendant une expérience.

## Provenance et changements

Patch fourni : `claude-alloc-aplat-20261125.patch`, SHA-256
`a9c0aee1d7949b17b84216e8bc6e143a5b827f24226fa9e26d428f60469293ba`.
Il comprend le précédent patch p99 ; il n'a pas été appliqué deux fois.
Claude Code a proposé le code et ses premières mesures ; Codex a revu les
chemins, ajouté des régressions et intégré les corrections suivantes.

| Commit | Changement |
| --- | --- |
| `769e815` | Cache du contrôle par séquence visible ; transactions à maps paresseuses ; tests de commit concurrent et réouverture |
| `330a5fc` | Parcours initial de 32 lignes, croissance jusqu'à 1 024 ; scratch réutilisé ; tests MVCC, pagination et résultats détachés |
| `d1c4b19` | Cache de sommes par pointeurs ; buffers rendus aussi au changement de version |
| `ee726a7` | Flush en étapes nommées ; destination d'ACK effectivement enregistrée ; première erreur de valorisation gardant sa reprise |
| `360538b` | Perte du cache parent après ACK, suppression des pushes inchangés et réparation exacte par polls |
| `5d93e2c` | Relais aplati et indices régionaux ; 1 024 emplacements et au plus 128 Kio de chaînes copiées, soit environ 208 Kio retenus sur hôte 64 bits |
| `1afbd3d` | Lots adaptatifs initiaux de 16 zones, minimum 4, objectif de 40 ms et hystérésis 20/80 ms |

La cible de 40 ms n'est pas une borne de gel : réduire un lot augmente le nombre
de commits fixes. Les indices de route ne donnent toujours aucun droit.
Les copies et empreintes restent liées aux permissions installées sur disque.
Les hints par cellule ajoutés au labo restent optionnels ; ils ne sont pas
activés silencieusement dans les comparaisons.

Les chiffres 48,4→20,6 Gio, 307→180 millions d'objets et p99 252→192 ms
proviennent du laboratoire **profilé de Claude sur deux cœurs**. Ils ne sont
ni une nouvelle mesure sur le portable de Codex ni un résultat AWS.
Le test local d'allocation confirme environ 709 octets par lecture `Children`
et 587 octets pour un parcours arrêté à sa première ligne, pool chaud.

## Qualification locale

- `go test -p 1 ./...`, `go build ./...`, `go vet ./...` : réussis.
- Audit des couches : zéro violation.
- Storage et zonelog : suites avec `-race` et vérification exhaustive des octets
  de capacité activée ; réussies (93,94 s et 197,93 s).
- Summary : suite `-race` réussie ; régressions avec perte du cache parent,
  redémarrage d'enfant et écriture pendant un push réussies sur trois passages.
- Mesh : suite complète `-race` réussie en 92,62 s avec
  `INDEXUS_BENCH_NO_USAGE_SCAN=1`, donc avec la comptabilité incrémentale de
  production. Les assertions d'autorité, reçus, reprises et scellés restent actives.

La première suite normale exécutée en concurrence avec ces grosses suites
`-race` avait dépassé le délai du test de grain ; elle est conservée comme échec,
puis la suite normale isolée passe sans modifier ce délai. La suite mesh
`-race` **avec les scans exhaustifs de diagnostic à chaque transaction** a été
interrompue après 377 s : elle n'est pas déclarée verte. Le test des 3 000 zones
multipliait scans de tout le disque et petits lots ; le même test, mêmes données
et assertions, passe en 37,18 s avec la comptabilité de production. Ces scans
supplémentaires sont propres aux tests ; ce constat n'explique pas les latences AWS.

## Campagnes et décisions de l'utilisateur

Configuration commune : DIRECT, H3, 2k→4k→8k→16k écritures/s,
120 s par plateau, 60 s de rampe, 180 s de queue, `i4i.xlarge`, deux slots par
machine, un loader `c6i.4xlarge`, seuil de 500 écritures/s/vnode et `ITEMS=0`.
Départ : deux machines/quatre vnodes ; maximum : 32 machines/64 vnodes.
Enveloppe de dimensionnement : 11 USD par essai, TTL 45 minutes. Cette enveloppe
n'est pas un coupe-circuit de facturation. Prix interrogés auprès d'AWS :
0,398 USD/h par hôte et 0,808 USD/h pour le loader.

| Run | Source | État |
| --- | --- | --- |
| `aws-alloc-before-1126-051744` | `2362b35` | Arrêt demandé après décrochage vers 8k/s ; traces partielles, pas d'audit final |
| `aws-alloc-after-1126-052554` | `1afbd3d` | Arrêt lors du démarrage à la même demande ; aucune charge comparative exploitable |
| `aws-alloc-new-1126-052820` | `1afbd3d` | Relance demandée, puis arrêt après un nouveau décrochage vers 6–8k/s ; traces partielles sans audit final |

Les vingt-trois instances des trois runs ont été terminées et leurs
launch templates supprimés. L'inventaire final ne trouve plus de VPC, sous-réseau,
groupe de sécurité, passerelle, rôle ou profil IAM appartenant à ces essais. Les objets S3 ont été récupérés avant suppression
des ressources. Les fragments `steps-1` de la référence ont été reconstruits
avec offsets contigus, sans trou ; cela ne remplace pas son audit manquant.
La référence arrêtée ne permet pas une comparaison des campagnes complètes.

## Ce que montre déjà la référence arrêtée

À environ 388 s de charge : 8 000 demandes/s, 3 124 ACK/s, 4 876 arrivées abandonnées/s par le générateur (file pleine),
p99 dans la classe logarithmique ≤8 388,608 ms, quatre transferts ouverts.
Ces abandons ne sont ni des refus HTTP 503 ni des ACK perdus : les requêtes
correspondantes n'ont pas été envoyées. Le run n'a pas été audité jusqu'à sa fin. Les relais restent proches de zéro et le loader ne
sature pas à cet instant.

Le CPU affiché par vnode est celui de son processus, normalisé par les quatre
CPU de l'hôte. Il faut additionner les deux slots au même instant. À environ
334 s, les processus d'un hôte atteignent ensemble **96,2 %** de sa capacité.
Une analyse qui ne regarderait que chaque vnode manquerait cette saturation.
La fenêtre n'est donc pas valide pour attribuer le p99 au seul nombre de nœuds.

### Démarrage du processus, autorité, monitoring et join sont distincts

Sur quatre slots de deux machines, les logs montrent des premiers ACK entre
0,57 et 1,31 s après lancement, puis monitoring et jobs seulement **120 s plus
tard**, après expiration du join initial. Dans `internal/app/node/main.go`,
le serveur p2p commence avant `clusterBoot.Start`, mais les jobs et le serveur
monitoring sont lancés après son retour. Le hook de spawn attend implicitement
`/status`, avec des délais qui s'emboîtent mal : demande autoscale 15 s,
hook 60 s, join initial 120 s.

```text
p2p disponible → runtime initialisé → premières zones installées → ACK possibles
                         ↓
                 attente du join complet
                         ↓ jusqu'à 120 s
                  monitoring + jobs
```

Cela montre un couplage de cycle de vie réel ; cela ne mesure pas à lui seul sa
part dans la perte de débit. Les machines de réserve sans logs de vnode ne sont
pas automatiquement en panne : des logs de provisioner vides sont publiés après
préparation de l'agent de machine. **22100 désigne l'agent ; 19100/19101 les
moniteurs des slots.** Un slot vide peut donc refuser la connexion normalement.
Le collecteur ne récupère pas cloud-init ni le journal complet de cet agent ;
on ne peut pas affirmer un échec de disque ou de téléchargement.

Le correctif de cycle de vie n'est pas inclus dans le binaire mesuré. Il faudrait
publier monitoring et workers après l'initialisation sûre (`b.ready.Store(true)`),
avant l'attente du join, conserver `/health` à 503 avant `ClientReady`, et séparer
l'ACK de lancement `/spawn` de l'attente explicite `wait=ready`. Déplacer les
jobs avant toute initialisation créerait un risque de lecture concurrente de
`node.mesh`. Leur annulation sur erreur de startup doit également être testée.

## Nettoyage Git

Cinq branches locales absorbées ou entièrement patch-équivalentes ont été
retirées. Les anciennes histoires quickstart et Docker sont conservées dans
les tags `archive/2026-11-26/*`, poussés sur le dépôt. Les deux anciennes branches
distantes d'autorité et de transfert, désormais contenues dans `main`, sont retirées.
Les branches avec commits uniques et le worktree de recherche modifié sont
préservés, ainsi que tous les fichiers non suivis du dossier principal.
Les dossiers d'expériences restent disponibles en worktrees détachés pendant
la collecte et pour conserver les traces ignorées.

Les manifestes, rapports détaillés, journaux et données brutes sont dans
`outputs/aws-claude-comparison-20261126/` du workspace. Les fichiers `all-runs-stop-verification.json` et le manifeste conservent
l'inventaire final. Aucune campagne ne s'est terminée par un audit complet :
aucune conclusion de durabilité n'est tirée de ces essais interrompus.


## Diagnostic du second décrochage

La version intégrée échoue également pendant la rampe 4k→8k. À t≈343 s,
6 667 demandes/s produisent environ 3 214 ACK/s ; les 16 000 workers du loader
sont occupés et 63 109 requêtes attendent. Puis la file de 64 000 est pleine et
le générateur abandonne les nouvelles arrivées. À t≈419 s : 8 000 demandes/s,
3 093 ACK/s, 4 907 abandons/s, zéro réponse 503 observée et p99 client dans la
classe ≤8 388,608 ms. La latence intentionnelle atteint une classe ≤33 554,432 ms.

### Receveurs : coût local sous writer, avant le commit

Sur un des receveurs, les différences de compteurs sur environ 30 s donnent :

| Poste | Slot A | Slot B |
| --- | ---: | ---: |
| Writer tenu, durée cumulée | 29,79 s | 29,92 s |
| Collecte | 10,54 s / 132 transactions | 10,32 s / 190 transactions |
| Contrôle | 11,94 s / 62 transactions | 12,49 s / 117 transactions |
| Montage | 6,69 s / 58 transactions | 6,56 s / 115 transactions |
| Écritures client | 0,62 s / 117 transactions | 0,55 s / 176 transactions |
| `validation`, avant `commitSpatial` | 28,81 s | 28,64 s |

Les catégories sont disjointes ; les durées des deux slots se chevauchent et ne
forment pas 60 s d'attente d'une seule requête. `validation` mesure `tx.run` et
`accountUsage` : cela comprend du calcul et potentiellement des lectures, pas
uniquement du CPU. Les captures établissent un writer occupé environ 99 % du
temps par contrôle, collecte et montage. Elles ne montrent pas un fsync tenant
ce writer pendant les 28,8 s.

Sur la fenêtre commune 344–364 s, les deux nouveaux hôtes atteignent **86,7 % et
88,6 % de CPU** en additionnant leurs deux slots, contre environ 28–49 % sur
les quatre anciens. Le placement de deux receveurs chauds sur le même hôte
amplifie donc la pression. Les CPU avant ouverture des moniteurs manquent :
on ne les remplace pas par zéro. Le plafond de 32 machines n'est pas atteint.

Les stacks disponibles sont celles des donneurs : ils attendent des réponses
H3 de preload/install des mêmes receveurs, et leurs reprises attendent la porte
du droit concerné. Les stacks receveurs et le profil CPU du loader manquent.
Les huit secondes côté client peuvent inclure transport, attente avant handler,
reprises et backoffs ; elles ne sont pas assimilées à une seule fence.

### Chemins précis restant sous le writer

`internal/storage/staging.go:280` contient encore `compactStaging` : recherche
jusqu'à 256 clés sous writer, puis `tx.del`, avec reprise depuis le début du
préfixe à chaque lot. Le plafond borne les suppressions, pas les clés ou
anciennes lignes retraversées. Une amplification quadratique est un candidat
à mesurer, pas une complexité observée et prouvée par ce run.

`compactZoneRows`, dans `zone_states.go:181`, utilise déjà un curseur monotone
et une page MVCC avant le commit de suppression ; son commentaire conserve
l'ancien incident de 10–28 s. La passe précédente n'avait donc pas uniformisé
ce pattern jusqu'à `compactStaging`.

`PreloadCausal` prépare aussi ses snapshots dans la transaction. Les lectures
résiduelles du montage et la sérialisation des droits/transferts locaux restent
à isoler. Le patch de buffers et de cache ne supprime pas ces chemins.
Les lots plus petits augmentent leur fréquence si le coût fixe reste élevé.

### Deux amplificateurs de la réaction d'élasticité

Le second run confirme le couplage du démarrage : première vague, ACK en
0,33–0,76 s mais monitoring/jobs après 30–43 s ; seconde vague, ACK en
0,50–0,69 s mais monitoring/jobs après **120 s exactement**. Le provisioner peut
attendre sa réponse alors que le vnode traite déjà une partie des écritures.
Cela rend l'offre de capacité observable et pilotable trop tard.

Par ailleurs `Service.Demand` est normalement incrémenté **après Apply réussi**,
et `AutoscaleTick` fournit `Queue: 0`. Il suit donc principalement les
complétions, sans représenter la file réelle des arrivées. Ce mécanisme peut
sous-réagir lorsque le débit servi ralentit ; sa responsabilité quantitative
reste à établir avec des deltas d'ingress/completion. Les jauges
`autoscale.pressure` peuvent rester anciennes quand le garde des transferts
retourne avant `Controller.Tick` : elles ne sont pas sommées comme mesure
instantanée de toute la demande.

### La jauge frozen ne mesure pas la fenêtre sans écrivain

`FrozenOutgoing`, `zone_states.go:297`, sélectionne toute opération Moving non
terminée. Il ne compte pas ses lots réellement fencés non installés.
À t≈342 s, les quatre opérations comptées sont en warm-copy/install depuis
18–62 ms, avec des âges d'opération de 12–25 s. Cela ne prouve pas un gel
continu de 12–25 s. Il faut séparer opérations ouvertes, fences non installées,
âge de ces fences et lots déjà installés en attente de bookkeeping.

### Corrections prioritaires à valider avant un nouvel essai

1. Paginer la collecte du staging avec curseur hors writer, revalider la phase
   puis supprimer des clés connues ; limiter sa cadence face aux écritures.
   Terminer la préparation preload/montage hors writer avec revalidation locale.
2. Découpler runtime initialisé, monitoring, jobs, `ClientReady` et join terminé.
   Acquitter `/spawn` sans `wait=ready` au lancement réussi ; respecter une vraie
   deadline pour l'attente explicite. Garder les contrôles d'autorité inchangés.
3. Séparer arrivées admissibles, complétions et pression de file dans le signal
   d'élasticité, sans compter plusieurs fois les relais. Rendre les snapshots
   de pression horodatés et agréger le CPU des slots par machine.
4. Mesurer l'admission et le placement des receveurs chauds, puis la vraie fenêtre
   de fence. Ne pas compenser ces chemins par un simple relèvement du plafond.

Ces points sont un diagnostic et un ordre de correction, pas des correctifs
appliqués au protocole après l'essai. Les binaires des expériences restent
identifiés par leurs commits d'origine. Aucune relance automatique n'est prévue.
