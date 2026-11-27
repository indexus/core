# Brief : rendre un split rapide sans perdre ce qui fait Indexus

**Statut : proposé.** Note de recherche.

Statut : document de travail, 2026-11-18. Écrit pour être confié à d'autres
modèles ou chercheurs qui ne connaissent pas le projet. But : chercher dans la
littérature comment les systèmes comparables font, et quelles idées on peut
reprendre **sans casser les fondamentaux listés en section 3**.

Branche : `feat/convergent-replication` (sur `perf/scale-64`). Les documents de
référence sont cités en fin de texte ; ce brief se suffit à lui-même.

---

## 1. Indexus en une page

Indexus est une base de données géospatiale **pair à pair** écrite en Go.

**Données.** Une *zone* est l'unité de stockage et d'autorité. Son identité
est `(collection, chemin)` ; son identifiant géométrique est un mot de 96 bits :
`id(z) = bits(position) || suffixe(collection)`. Les zones forment un arbre
(une zone parent couvre ses enfants) ; des agrégats exacts (comptes, sommes)
remontent l'arbre. Un parent et un enfant peuvent avoir le même ID géométrique.

**Qui possède quoi.** Les nœuds logiques s'appellent *vnodes* ; chacun a un ID
de 96 bits. Le propriétaire *désiré* d'une zone est une fonction pure :

```text
Owner(z, V) = argmin_{v ∈ V} XOR(id(z), id(v))      (style Kademlia)
```

`V` est l'ensemble des vnodes actifs, tel que **chaque nœud le connaît
localement**. Il n'y a ni maître, ni annuaire, ni service de placement.

**Connaissance (plan de contrôle).** L'ensemble actif est un **CRDT** : un
ensemble croissant de faits (activations, retraits), fusionné par union,
propagé par gossip. Les vues peuvent diverger temporairement ; elles
convergent sous livraison éventuelle. Aucun délai n'est promis.

**Autorité d'écriture (le point central).** Une vue XOR n'est qu'une
*prédiction de routage*. Le droit d'**acquitter** une écriture vient d'un
*grant* : une région de l'espace 96 bits `(mask, value)`, y compris l'espace
sans données, détenue par un disque précis (vnode + incarnation de disque).

- Un *certificat* consomme un jeton et partitionne sa région en sorties
  disjointes et complètes. Chaque jeton a **au plus un successeur**.
- Le jeton consommé est persisté par la source **avant** l'émission ; après
  redémarrage, la source reste fermée.
- Le gossip apprend les certificats mais **n'installe jamais** d'autorité. Le
  receveur doit monter les octets, les reçus et le grant dans **une seule
  transaction disque**.
- Le contrôle d'autorité est fait **dans la transaction de commit** de
  l'écriture. Une écriture reçue par le mauvais nœud est **relayée** au
  détenteur (au plus 64 sauts), jamais acquittée sur la foi d'une vue.

Conséquence : **au plus un écrivain par zone à tout instant**, sans consensus,
sans bail, sans horloge, avec une interruption possible (zone sans écrivain)
entre la fermeture côté source et l'installation côté receveur.

**Mise à l'échelle.** Quand un vnode est trop chargé (ici : trop d'éléments,
seuil simulé à 20 000), il se *split* (HotTrie) :

```text
g(D) = 1 + bit contraint le plus profond du grant de D      (la "garde")
J    = D xor e_b,  pour un bit b ≥ g(D)                       (nouvel ID)
zones déplacées = { z dans le grant de D : z[b] ≠ D[b] }
```

Théorème HOT-1 (prouvé et testé) : ce split ne change la cellule XOR d'aucun
autre vnode. Il est **donneur seulement**, local, et commute avec les autres
splits. Le politique choisit le bit le moins profond qui laisse au moins 20 %
de chaque côté.

**Important pour la suite :** un split crée `J` dans un **autre processus**
(souvent une autre machine). Dans Indexus aujourd'hui, *split = déplacement de
données*. La moitié environ des zones de `D` doivent physiquement partir.

## 2. Le protocole de transfert actuel (handoff)

L'autorité bascule **atomiquement par zone**, pas par région. Raison : la région
déplacée contient des parents et leurs enfants ; une bascule de région entière
fermerait toute la région le temps de la phase finale, et déplacerait un parent
avec ses enfants d'un bloc. La bascule par zone garde les zones sœurs
inscriptibles.

Trois faits distincts par zone :

| Fait | Sens |
| --- | --- |
| `DesiredOwner(z)` | XOR sur la vue locale. Indice de routage. |
| `Authority(z)` | Le disque qui peut acquitter `z` : détenteur du grant, remplacé par le `To` du dernier `ZoneTransfer(z)` tant que le certificat de région n'a pas consommé ce grant. |
| `Residual(z)` | Octets restés chez la source après bascule ; sans autorité ; collectés après la fin. |

Déroulé d'un split (une *opération*) :

```text
1. JoinIntent persisté (le plan exact), MigrationSet capturé ; la source reste inscriptible.
2. Copie chaude + miroir de toutes les zones, en parallèle, par lots de 512 zones
   par requête. La source continue d'acquitter ; le receveur rattrape par séquence.
3. Phase finale, par niveaux du DAG (feuilles d'abord, ZONE-2 : un parent bascule
   après ses enfants), par LOTS DE 64 ZONES, chaque lot en 5 étapes séquentielles :
     a. FreezeZones (tx locale source)      la zone ne peut plus être écrite chez la source
     b. miroir final   (aller-retour réseau) dernier delta jusqu'à la séquence gelée
     c. stage-zone     (aller-retour réseau) le receveur scelle la copie, vérifie le DataDigest
     d. EmitZoneTransfers (tx locale source) fait irréversible : Authority(z) = receveur
     e. install-zone   (aller-retour réseau) le receveur monte z et peut acquitter
   Entre (a) et (e), la zone n'a AUCUN écrivain : les écritures sur ces 64 zones attendent.
   Dès la première zone installée, J publie son activation (ACT-1) et reçoit du trafic.
4. FreezeRegion : plus aucune zone nouvelle chez la source.
5. Certificat de région (un seul, consolide les ZoneTransfer), installé chez le receveur.
6. GC des résidus côté source, par lots, hors chemin critique.
```

Les reprises conservent l'identité de l'opération et la progression ; une zone
gelée ne dégèle jamais sur timeout. Les `ZoneTransfer` restent entre source et
receveur (ZONE-GOSSIP-1) et sont stockés comme lignes locales (XFER-KEYS-1),
pour que la connaissance gossipée ne grossisse pas avec le nombre de zones.

Code : `internal/mesh/service.go`, `transferZones` (boucle des 5 étapes) ;
`internal/storage/causal_zone.go` (freeze, emit, install).

## 3. Les fondamentaux à garder (non négociables)

1. **Pas de maître, pas de consensus, pas de bail, pas d'horloge
   synchronisée.** Pas de verrou global, pas de validation globale.
2. **`Owner = argmin XOR` reste pur.** Charge, chaleur, placement physique
   n'entrent jamais dans `Owner`.
3. **Au plus un écrivain par zone** (AUTH-1..4, HND-3). L'autorité se prouve
   par des certificats à successeur unique, persistés avant émission, et se
   vérifie dans la transaction de commit.
4. **Une vue périmée ne donne jamais l'autorité** (ROUTE-1). Le gossip
   n'installe rien.
5. **Une écriture acquittée survit** (HND-2) : restart, retry, messages en
   retard (un vieux `install-zone` ne remonte jamais d'anciens octets).
6. **Coût de contrôle proportionnel au changement, jamais à l'historique** ;
   connaissance gossipée en O(vnodes), pas O(zones déplacées).
7. **Agrégats exacts** une fois stabilisés ; hiérarchie parent/enfant
   respectée (ZONE-2, MIG-1).
8. **Simplicité** : un seul mécanisme réutilisé (le même transfert sert au
   split, au MOVE, au fold, et demain à la réplication), peu de code, des
   invariants nommés et testés.

On accepte : une courte fenêtre sans écrivain sur une zone, des vues
divergentes, une convergence sans borne de temps promise.

## 4. Où on en est (mesures)

Le scénario d'élasticité : base 200–400 écritures/s, puis ×8 à ×10 sur un
préfixe pendant 2 min, puis 7 min de calme (les vnodes froids doivent se
replier).

**Critère vert :** chaque split finit (ou avorte proprement) en **moins de
2 minutes**, aucune zone laissée gelée, audit sans perte ni double écrivain,
nombre de vnodes revenu à ±1 après le calme.

| | Avant (17 nov) | Aujourd'hui |
| --- | --- | --- |
| Écritures pendant le pic | 0/s, cluster bloqué | tenues (≈2 000/s local, 3 200/s sur 2 hôtes AWS pendant 2 min) |
| Écritures acquittées / offertes (AWS) | 439 559 / 458 800 | 456 635 / 458 800 |
| Connaissance gossipée | jusqu'à 15,5 Mo | ≤ 12 ko |
| Trafic entre nœuds (AWS) | 5,6 Mo/s/nœud | 0,45 Mo/s/nœud |
| Coût d'install par lot de 64 zones | croissait (54 → 750 ms) | plat (52 → 61 ms) |
| **Durée d'un split** | jamais fini | **7 à 10 min sur AWS, ≈11 min en local** |
| Audit (3ᵉ campagne AWS) | — | 20 000 clés relues, 0 perte, 0 double écrivain |

Ce qui a été corrigé (cascade de verrous, connaissance qui gonflait, budgets de
maintenance, scans complets à chaque lot) faisait que le coût **croissait** avec
le transfert. Il est maintenant **constant par lot**. Le problème restant est
d'une autre nature : **un coût fixe par lot × beaucoup de lots**.

Ordres de grandeur, en local (7 processus sur un seul Mac) : un split déplace
≈25 000–30 000 zones ; ≈45 zones/s ; ≈1,4 s par lot de 64 zones, dont
seulement ≈60 ms d'installation. Un run a fini ≈25 800 zones en ≈3 min,
un autre a pris 11 min : la variance est forte et pas expliquée. Biais connu :
en local, sources, receveurs et charge partagent les mêmes cœurs ; sur AWS
(2 hôtes), le split reste à 7–10 min.

## 5. Le problème à résoudre maintenant

**Énoncé court :** un split sous charge prend 7–10 minutes ; il en faut moins
de 2. Pendant ce temps la capacité nouvelle n'est pas pleinement utilisable,
et la source porte le pic seule.

Décomposition :

- **Le nombre de zones déplacées est proportionnel aux données.** Avec des
  données diffuses (une ligne par zone), un split déplace des dizaines de
  milliers de zones. Chaque zone est un objet d'autorité : gel, preuve,
  installation.
- **La phase finale est sérielle par lot.** 5 étapes, dont 3 allers-retours
  réseau et 2 transactions disque, sans recouvrement d'un lot sur le suivant.
  Taille de lot fixe à 64. Débit ≈ 64 / (latence d'un lot).
- **Chaque lot ouvre une fenêtre sans écrivain** sur ses 64 zones ; des lots
  plus gros allongent la fenêtre de chaque zone.
- **Split = déplacement.** Aucune capacité n'est gagnée avant que les octets
  aient voyagé, alors que le split *logique* (HOT-1) est purement local.

Symptômes secondaires, probablement liés :

1. Un transfert reste ouvert sur certains nœuds jusqu'à la fin du run.
2. Les nœuds en transfert répondent lentement au monitoring.
3. En local, après le pic, des pauses d'écriture de 1 à 5 s toutes les 10–20 s
   (non expliquées).

Pistes déjà sur la table (non mesurées) :

- **Pipeline** : lancer le lot `k+1` sans attendre la fin du lot `k`
  (fenêtre glissante), en respectant ZONE-2 entre niveaux.
- **Lots plus gros** (un essai local à 256 donnait ≈1,7×).
- **Fusionner des étapes** : par exemple porter le dernier delta dans
  `stage-zone`, ou `EmitZoneTransfer` dans la même réponse que l'install.

Ces pistes gagnent un facteur constant. La question de fond est de savoir si
l'on peut **ne plus payer par zone**.

## 6. Questions pour la recherche bibliographique

Pour chaque idée trouvée : qui la fait, comment, quel coût, et **est-elle
compatible avec la section 3** (sinon, quelle propriété elle casse).

**Q1. Découpler le split logique du déplacement physique.**
Dans Bigtable/HBase, CockroachDB, TiKV, un split est (à vérifier) une opération
de métadonnées sans déplacer d'octets ; le déplacement est ensuite un problème
séparé (rééquilibrage par snapshot entier d'une plage). Indexus a déjà un
`MOVE` (même vnode, autre machine, même propriété XOR). Peut-on faire :
split local sur le même disque (certificat sans octets), puis `MOVE` du
nouveau vnode d'un bloc ? Que coûte un MOVE (est-il lui aussi par zone ?) ?
Quel est l'équivalent dans la littérature (HBase reference files,
Ceph split de PG puis backfill, Elasticsearch shard split par hard links puis
relocation) ?

**Q2. Granularité de la bascule d'autorité.**
Par zone (aujourd'hui) contre par région (un seul gel, un seul certificat,
fenêtre sans écrivain = durée du dernier delta). Comment les systèmes qui
déplacent une plage entière (relocation de shard primaire dans Elasticsearch,
transfert de leader Raft, MoveKeys de FoundationDB, déplacement de
répertoires Spanner) bornent-ils la fenêtre de blocage ? Est-ce qu'un
pré-copy bien rattrapé suffit pour que la fenêtre d'une région soit courte
(sous-seconde) ? Comment traiter alors ZONE-2 (parents/agrégats) ?

**Q3. Migration à chaud de bases de données.**
Littérature *live migration* : pré-copy (Albatross, Slacker), post-copy et
tirage à la demande (Zephyr, Squall dans H-Store), hybrides (ProRea). Idée
notable de Squall : basculer l'autorité **d'abord**, puis le receveur tire les
données à la demande et en fond. Compatible avec « au plus un écrivain » et
« une écriture acquittée survit » ? Que devient la lecture exacte pendant le
tirage ?

**Q4. Rendre le coût indépendant du nombre de zones.**
Une preuve par zone (`ZoneTransfer` avec `DataDigest`) est-elle nécessaire, ou
une preuve par plage avec un digest agrégé (arbre de Merkle sur la région)
suffit-elle ? Comment les systèmes qui transfèrent des millions de clés
certifient-ils l'intégrité (Merkle trees de Dynamo/Cassandra, snapshots
Raft) ?

**Q5. Choix du bit de split et forme de l'ensemble déplacé.**
`J = D xor e_b` déplace `{z : z[b] ≠ D[b]}`. Existe-t-il une politique de choix
de `b`, ou de pré-découpage (vnodes virtuels dès le départ, comme les vnodes
de Dynamo/Cassandra), qui réduit le volume à déplacer au moment du pic, sans
casser HOT-1 ? Exemple : avoir plusieurs vnodes par processus et ne déplacer
que des vnodes entiers.

**Q6. Pipelining et contrôle de flux.**
Formes classiques (fenêtre glissante, group commit, batching adaptatif) pour
une suite d'étapes dont certaines sont irréversibles (`Emit`). Comment éviter
qu'un pipeline viole l'ordre parent-après-enfants ?

**Q7. Transfert sous charge.**
Comment limiter l'impact d'un transfert sur les écritures et le monitoring du
nœud (priorités, budgets d'E/S, isolement CPU) ? Comment les systèmes
détectent-ils et reprennent-ils un transfert qui ne progresse plus ?

## 7. Ce qui n'est pas demandé

- Remplacer le modèle par du consensus (Raft/Paxos par plage), un maître de
  placement ou des baux : hors ligne (section 3). Une idée venue d'un système à
  consensus est bienvenue **si son mécanisme survit sans le consensus**.
- La réplication (zone sur 3 vnodes, fusion convergente après partition) est
  un chantier séparé, en conception (`docs/design/convergent-replication*.md`).
  Mais une solution de transfert qui sert aussi la réplication (une réplique =
  un receveur qui n'installe jamais) a une valeur double.

## 8. Livrable attendu d'un modèle chercheur

1. Pour chaque question Q1–Q7 : 2 à 5 références primaires (papier, doc
   officielle, code), une phrase sur le mécanisme, son coût.
2. Un verdict par idée : compatible / compatible avec ajustement (lequel) /
   incompatible (quelle propriété de la section 3 casse).
3. Une recommandation ordonnée : ce qui donne le plus pour le moins de code,
   avec un argument de sûreté (quel invariant reste vrai, pourquoi).
4. Pas d'implémentation : une explication assez précise pour qu'un autre
   modèle puisse l'écrire.

## 9. Références internes

| Sujet | Fichier |
| --- | --- |
| Transfert par zone, phases | `docs/protocol/handoff.md` |
| Autorité, certificats, CRDT | `docs/protocol/causal-topology.md`, `docs/protocol/write-authority.md` |
| Propriété XOR, split HotTrie | `docs/protocol/ownership.md`, `docs/protocol/scaling.md` |
| Théorèmes (HOT-1, frontière CRDT) | `docs/protocol/linear-knowledge.md` |
| Liste des invariants | `docs/protocol/invariants.md` |
| Transferts hors gossip | `docs/protocol/zone-transfer-scope.md`, `docs/protocol/transfer-state-keys.md` |
| Mesures d'élasticité | `docs/benchmarks/elasticity.md` |
| Problèmes connus | `docs/benchmarks/p2p-findings-2026-11-15.md` |
| Positionnement dans l'état de l'art | `docs/research/state-of-the-art.md` |
| Boucle des 5 étapes | `internal/mesh/service.go` (`transferZones`) |
