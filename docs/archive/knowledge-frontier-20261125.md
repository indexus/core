# Knowledge

**Statut : historique — instantané du protocole à checkpoints avant son remplacement.** Code : `internal/mesh/knowledge_wire.go`,
`internal/mesh/service.go` (`Handle`), `exchangeKnowledge`, `exchangeWrite` et `exchangePrepared` dans `knowledge_wire.go`,
`internal/storage/causal_data.go` (`MergeCausal`).
Règles : ROUTE-1, KNOW-LAZY, KNOW-FAST, KNOW-CARRY-1, XFER-KEYS-1, GOS-1,
ACT-1 ([registre](../protocol/invariants.md)). Théorie :
[../protocol/linear-knowledge.md](../protocol/linear-knowledge.md).

**Branche authority-frontier, 24 novembre 2026.** La compaction ajoute un
checkpoint de droits courants et un contexte causal compact. Elle change
explicitement la confiance accordée aux preuves anciennes ; le transport
actuel est conservé à cette première étape. Voir le
[contrat et ses limites](../protocol/authority-frontier.md).

**25 novembre : reprise après perte d’ACK.** Une ancienne installation devenue
obsolète peut être acquittée sans remonter les données ni réactiver un droit.
Le contexte de consommation du checkpoint suffit ; aucun journal supplémentaire
n’est ajouté. Voir le [cas reproduit et sa vérification](../protocol/handoff-reliability.md).

## Ce que c'est

Knowledge est l'ensemble des faits causaux validés et des preuves qu'un nœud connaît
(`mesh.Knowledge`) :

- `Root`, `Genesis` : le premier vnode et l'identité du cluster ;
- `Delta.Intents`, `Delta.Placements` : les intentions de join, et quel vnode
  tourne sur quel disque ;
- `Delta.Activations` : les vnodes actifs, chacun avec la preuve qu'il a acquis
  une partie de sa cellule (ACT-1) ;
- `Delta.Certificates` : les certificats régionaux émis ; leur présence
  ne prouve pas que le receveur a installé les données ;
- `Delta.ZoneTransfers` : seulement les transferts de zone qu'une activation
  cite comme preuve ;
- `Delta.Retirements` : les vnodes retirés.

Chaque nœud en tire sa vue (`tree.View` : les vnodes actifs) et son réplica
d'autorité (qui détient quelle région). `WriteTarget` route par cellule XOR dans
cette vue. `ZoneHolder` résout le détenteur d'une zone à partir des grants du
réplica et des lignes de transfert de ce disque.

Les transferts de zone d'un handoff en cours **ne sont pas** dans Knowledge.
Ils restent en lignes sur la source et le receveur (XFER-KEYS-1) : un lot de
bascule ne change donc pas la version de Knowledge.

## Ce qu'il autorise

**Garantie (ROUTE-1).** Recevoir Knowledge n'installe aucune donnée et ne suffit
jamais à autoriser une écriture. Seul l'état durable du disque qui écrit,
vérifié dans sa transaction, permet d'acquitter (AUTH-3).

Les faits de Knowledge **participent** pourtant aux contrôles :

- un certificat consomme un grant ;
- une activation fait entrer un vnode dans la vue ;
- un placement dit sur quel disque un vnode sert.

Un nœud dont Knowledge est en retard route mal, et le disque visé refuse ou
relaie. Il ne peut pas pour autant écrire à tort.

## Versions

La **trame** d'un nœud est son Knowledge encodé en JSON. Sa **version** est le
sha256 de ces octets (`makeKnowledgeFrame`).

Elle est reconstruite quand les faits durables changent, c'est-à-dire quand
`FactsDigest` change. Une vérification de la trame en cache n'attend jamais une
reconstruction en cours (KNOW-FAST) : seule la reconstruction est sérialisée
(`knowledgeWire.build`).

La version change à chaque fait nouveau : intention, placement, activation,
certificat ou retrait. Pendant une montée en charge, elle change donc à chaque
vague de joins.

## Transport entre pairs

Il y a quatre chemins, selon la requête. Les ajouts de transport incrémental
et de projection des résumés sont validés dans le worktree de récupération
2026-11-24 ; leur qualification à grande échelle reste décrite dans le rapport
de benchmark.

**1. Les écritures** (`exchangeWrite`, depuis 9590cba et d5e9873). Une écriture
relayée ne porte ni paquet ni référence obligatoire. Elle envoie seulement
`KnownKnowledge`, le hash d'une trame déjà construite, peut-être périmé ou nul,
et demande une réponse compacte (`AsyncKnowledge`). Le receveur vérifie
l'autorité dans la transaction du commit et répond le résultat, le disque qui a
appliqué, et au besoin `KnowledgeStale`. Il ne construit ni n'attache de
Knowledge, pas même sur un refus.

Si l'une des deux parties est en retard, l'appelant lance un échange **en
arrière-plan** (`syncKnowledgeAsync`) :

- au plus un échange par pair, et huit par vnode, nouveaux essais compris ;
- deux tentatives au plus, chacune avec un contexte de 5 s, et une courte
  attente après un échec ;
- à saturation, c'est l'indice qui est abandonné, pas l'écriture.
  `gossip.async_sync_dropped` et `async_sync_failures` les comptent.

L'API d'écriture du nœud (`meshWrite`) passe par ce chemin.

**2. Les étapes d'un lot gelé** (`exchangePrepared`). Le donneur échange les
dépendances causales **avant** de geler un lot. Ensuite, `mirror`,
`stage-zone` et `install-zone` passent en réponses compactes, sans attendre le
porteur de Knowledge. Une dépendance manquante fait échouer le lot : elle ne
s'installe jamais par gossip. Après la fin durable du join, un échange
synchrone dédié garde la barrière de convergence. Les zones installées
acceptent déjà les écritures pendant cet échange.

**3. Les résumés** (`exchangeSummary`). Une empreinte de membership et placement
(`RoutingDigest`) évite de synchroniser tous les certificats de données. Une
divergence refuse la contribution et déclenche une réparation asynchrone ; un
ancien pair utilise le chemin validé complet. Le chemin de lecture fraîche
avec inventaire conserve ses contrôles causaux locaux, sans bundle global ;
voir le chemin suivant.

**4. Les inventaires** suivent les sources et successeurs déclarés dans le même snapshot
MVCC que les lignes. Les émissions régionales conservent un pointeur local
après compaction des reçus ; les relais sont suivis récursivement. Ni la requête
ni la réponse ne portent de Knowledge globale, sans repli vers un ancien mode.
L’ensemble initial des détenteurs dépend encore de la frontière connue de
l’entrée. Voir [les garanties et limites](../protocol/read-delegations.md).

**5. Tous les autres appels** (`exchangeKnowledge`), y compris le handoff
lui-même et l’anti-entropie :

```mermaid
sequenceDiagram
  participant A as appelant
  participant B as pair
  A->>A: carryFull(B, V) : porteur, suiveur ou déjà connu
  alt B a confirmé V
    A->>B: requête + référence V
  else A est le porteur de V vers B
    A->>B: requête + paquet complet V
  else un autre appel porte V vers B
    A-->>A: attend la fin de cet appel (≤ 2 s)
    A->>B: référence si B a confirmé V, sinon paquet complet
  end
  B->>B: acceptKnowledge : fusionne avant toute action
  alt référence inconnue de B
    B-->>A: NeedKnowledge
    A->>B: même requête + paquet complet (une fois)
  end
  B-->>A: réponse + AcceptedKnowledge + Knowledge de B si A est en retard
  A->>A: fusionne la réponse ; note que B a V
```

**Garantie (KNOW-CARRY-1).** Sur ce quatrième chemin, pour un pair et une
version donnés, un appel est désigné porteur du paquet complet. Un appel
concurrent attend la fin du porteur pendant au plus `knowledgeCarryWait`
(2 s), puis envoie une référence si le pair a confirmé la version, ou le
paquet complet sinon, même si le premier porteur tourne encore. La règle
déduplique dans le cas courant, mais ne garantit pas un paquet unique en vol
après expiration du délai. C'est la parade à REM-121 : après un changement de
topologie, chaque appel concurrent portait le paquet entier.

**Mécanisme côté receveur** (`acceptKnowledge`) :

- **Paquet complet de version inconnue** : il est fusionné (`MergeCausal`)
  **avant** que la requête soit traitée. Les fusions d'une même version sont
  dédoublonnées (`mergeOnce`), et l'attente d'une fusion menée par un autre
  appel peut être annulée.
- **Référence inconnue** : refus `ErrDependency` avec `NeedKnowledge`. Ce refus
  n'a aucun effet sur les données, et l'appelant refait l'appel avec le paquet.

**Anti-entropie.** `Service.Sync` échange Knowledge avec un ensemble borné de
partenaires (GOS-1), même sans trafic client.

## Paquets incrémentaux négociés (worktree 2026-11-24)

Le diagramme précédent décrit le chemin complet de secours. Les pairs qui
annoncent `KnowledgeDeltas` utilisent désormais une base confirmée :

- un paquet complet conserve sa version `H(Knowledge)` ;
- un delta porte `KnowledgeBase` et une version
  `H("indexus-knowledge-delta-v1", base, corps)` ; cette version désigne un
  paquet avec dépendances, pas le hash d'un snapshot complet ;
- une référence ne porte aucun corps ; elle exige un reçu local déjà validé
  chez le destinataire. Au premier contact, on peut sonder une version complète
  déjà retenue localement : le pair peut l’avoir apprise ailleurs. Une référence
  inconnue est refusée avant dispatch et provoque le renvoi complet. Cela ne
  permet jamais d’envoyer un delta à un pair sans capacité négociée.

`knowledge_delta.go` mémorise la couverture de chaque reçu en identifiants de
faits `(type, digest)`. La couverture d'un delta est au plus l'union de la base et du
corps. Elle peut être réduite aux faits encore exportés : c'est une borne
inférieure conservatrice, pas une liste qui doit retenir tout le passé. Root et Genesis doivent correspondre à la base. La source calcule la
différence depuis la base partagée ; le receveur fusionne avec les validateurs
existants avant de confirmer. Il peut répondre par un delta depuis le paquet
reçu : l'appelant connaît déjà cette base. Plusieurs bases concurrentes sont
permises ; elles ne forment pas une horloge globale.

Le cache retient au plus 128 couvertures et 65 536 identifiants au total. Une
couverture individuelle dépassant ce budget n'est pas retenue. Les compteurs `delta_packets`, `full_packets`, `reference_packets` comptent
les paquets construits ; `delta_base_misses` et `coverage_evictions` expliquent
les replis. Les caches sont
volatils : éviction, redémarrage et absence de capacité négociée utilisent le
paquet complet. Une base absente provoque `NeedKnowledge` avant le dispatch et
une seule reprise complète. Une erreur sur une réponse déjà exécutée ne déclenche
pas de retry métier générique ajouté par ce mécanisme.

Les écritures, résumés et étapes déjà préparées restent sur leurs chemins
compacts. La comparaison des indices des écritures utilise toujours une version
de snapshot complet mémorisée : elle ne confond pas un reçu delta avec l'état
global. La réception des faits n'installe aucun grant. Une activation peut rester
pending selon les règles existantes ; la frontière validée ne saute pas de trou.

Le coût de fusion peut encore dépendre de l'historique, même pour un petit delta.
Cette étape optimise le transport ; elle ne prétend pas rendre les validateurs
inconditionnellement commutatifs face à des certificats contradictoires.

## Coût, et effet mesuré sur les écritures

Le paquet complet contient tout l'historique (`Knowledge(nil)`, c'est-à-dire
`DeltaSince(nil)`). Il grandit avec le nombre de joins. 96fddfb rapporte des
paquets de 166 Ko, et `metrics.Gossip.KnowledgeBytes` donne la taille
courante.

Jusqu'à 9590cba, une écriture relayée passait par le chemin synchrone complet. Juste
après un changement de version, elle pouvait attendre jusqu'à 2 s la fin d'un
autre appel vers le même pair, puis la fusion du paquet chez ce pair.

**Mesure locale, avant et après.** Le banc est `TestMeasuredJoinWritePath`
(`internal/mesh/join_write_path_measure_test.go`) :

- **Conditions** : 3 nœuds en mémoire, écritures qui entrent par un nœud tiers
  avec `AsyncKnowledge` comme `meshWrite`, un vnode qui rejoint sous charge,
  RTT simulé de 1 ms, aucun coût ajouté au paquet, 32 écrivains, 512 zones,
  3 runs de chaque côté le 24 novembre 2026 ;
- **Avant** : `ec5752b`, soit 742c1db plus les correctifs de revue, sans le
  champ `AsyncKnowledge` ;
- **Après** : d5e9873 plus le nettoyage ;
- **Commande** :
  `INDEXUS_JOIN_MEASURE=1 go test -run TestMeasuredJoinWritePath -v ./internal/mesh`.

| | avant | après |
| --- | --- | --- |
| latence max pendant le join | 2 016–2 023 ms | 302–331 ms |
| p99 pendant le join | 289–1 644 ms | 199–216 ms |
| p50 pendant le join | 5,1–5,3 ms | 6,3–7,5 ms |
| paquets Knowledge complets échangés | 16–46 | 11 |
| premier `install-zone` | 163–177 ms | 168–182 ms |
| première écriture reçue directement par le nouveau vnode | à la fin du join, 1 805–2 321 ms | à la fin du join, 2 259–2 527 ms |

Le plafond de 2 s a disparu, ce qui confirme qu'il venait de l'attente du
porteur. Le nœud d'entrée continuait en revanche de passer par la source
jusqu'au certificat de région.

**Indice de route au nœud d'entrée (WRITE-HINT-1, `bfb8432`).** Mesuré le 24 novembre
2026, 3 runs de chaque côté, même commande ; *sans* : main 6a3015e plus SUM-4,
*avec* : plus l'indice.

| | sans indice | avec indice |
| --- | --- | --- |
| durée du join | 2 399–2 919 ms | 2 406–3 355 ms |
| premier `install-zone` au receveur | 152–199 ms | 158–187 ms |
| première écriture relayée par la source au receveur (2 sauts) | 185–214 ms | 163–195 ms |
| première écriture reçue directement par le receveur (1 saut) | à la fin du join, 2 378–2 901 ms | 219–269 ms |
| écritures arrivées au receveur en 2 sauts | 1 320–1 602 | 530–586 |
| p50 pendant le join | 6,8–8,0 ms | 6,1–6,8 ms |
| p99 pendant le join | 207–253 ms | 224–311 ms |
| max pendant le join | 312–329 ms | 336–369 ms |

Dans ces runs, la première écriture directe arrive environ 60 ms après la
première installation, et les écritures en 2 sauts baissent d'environ 60 %.
Les p99 et maxima observés ne montrent pas de réduction avec l'indice. Trois
runs ne permettent pas d'attribuer leurs écarts au simple bruit ni d'isoler
toutes les sources d'attente. L'indice retire un détour (un aller-retour
simulé de 1 ms ici), pas les attentes de gel des lots.

Ces chiffres valent en mémoire, sans fsync ni vrai réseau. `handoffs.install_to_first_write_us_log2`
mesure en production le délai entre l'installation et la première écriture
appliquée localement.

## Pistes (statut : proposé)

Voir [chemin-ecriture.md](../architecture/chemin-ecriture.md#pistes-statut--proposé).
La [comparaison Lightning, Agave et CRDT](../research/knowledge-convergence.md)
sépare diffusion incrémentale des faits, indices de route et droits installés.
Elle distingue le transport incrémental branché dans ce worktree des évolutions
encore proposées (réception persistante pending, compaction, diffusion ciblée).
