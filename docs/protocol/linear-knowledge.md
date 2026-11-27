# Connaissance linéaire : théorie et exigences d'implémentation

> **Historique : protocole prédécesseur à preuves/checkpoints.** Le remplacement
> par [droits locaux et tickets](local-permissions.md) est branché dans le worktree
> le 25 novembre 2026 ; ses [preuves séparées](local-permissions-verification.md)
> ne reprennent pas automatiquement les résultats de cette page. Les mentions
> « actuel » ci-dessous appartiennent à cet instantané historique.

**Statut : actuel.** Théorie et exigences ; le transport réel est dans [knowledge.md](../architecture/knowledge.md).

Historique théorique au 14 novembre 2026 ; état du transport actualisé le
24 novembre 2026 dans le worktree de récupération. Ce document fixe la cible algorithmique du plan de
contrôle et ce qu'une implémentation doit respecter. Il complète
[invariants.md](invariants.md) ; il ne remplace aucun invariant de sûreté.

But : le coût de chaque opération de contrôle est proportionnel au changement,
jamais à l'historique. `N` = vnodes, `d` = taille du changement.

| Opération | Aujourd'hui | Cible |
| --- | --- | --- |
| Autoriser une écriture | O(profondeur) depuis l'étape 1 | O(profondeur) ≤ 96 |
| Fusionner un fait | Clones et validation pouvant encore dépendre de l’historique | O(d log N), aucune reconstruction |
| Taille de la connaissance | Membership et historique des certificats conservé | O(feuilles), compaction encore à qualifier |
| Propager un changement | Partenaires CPL ; deltas négociés et secours complets | Changements seuls à k voisins, sans historique répété |

La cible de coût proportionnel au changement n'est pas une garantie de
l'implémentation actuelle : le transport et le coût de validation se mesurent
séparément. Les reçus incrémentaux, leurs sondes par référence et les limites de
cache sont décrits dans [Knowledge](../architecture/knowledge.md). L'étude des
alternatives et la carte des suites sont dans
[knowledge-convergence.md](../research/knowledge-convergence.md). Les droits
installés et les fences ne sont pas déduits de ces reçus de transport.

## 1. Trois résultats

Chacun a un test exécutable dans `internal/causal`. Un test borné n'est pas une
preuve générale ; l'argument est donné pour être relu.

### T1 — La garde d'un hot split se lit sur le grant du donneur (`HOT-1`)

Énoncé. Soit `D` détenteur d'un unique grant terminal `R`, avec `ID(D) ∈ R`.
Soit `g = 1 + (bit contraint le plus profond de R)`, ou `0` si `R` est sans
contrainte. Pour tout `b ≥ g`, le split `J = D xor e_b`, qui cède
`R ∧ (bit b = J[b])`, ne change la cellule XOR d'aucun autre vnode.

Argument. Invariant `I` : chaque grant terminal est exactement la cellule XOR
de son détenteur. Les bits contraints de `R` sont alors les premiers bits
différents entre `D` et chaque concurrent. `J` ne diffère de `D` qu'en `b`, plus
profond que tous : face à tout concurrent `U`, `J` a le même premier bit
différent que `D`. `U` ne reçoit aucune contrainte nouvelle. La cellule de `J`
est celle de `D` plus le bit `b`. `I` est conservé. Deux splits de grants
différents ne touchent pas le même grant : ils commutent.

Conséquence. La garde n'a pas besoin de la vue. Tout vnode présent dans `R` y
est entré par un certificat émis par `D` : aucun concurrent ne peut être caché.
Le contre-exemple `TestHotSplitGuardCannotCertifyAnOmittedCompetitor` disparaît
dès que la garde vient du grant.

Limite. `I` ne tient pas pendant un ADD régional ou une jointure à ID
arbitraire, tant que `Rebalance` n'a pas fini. Un split en dessous de la garde
casse `I` : sur 2 bits, `D=01`, `J=00` (split au bit 1), puis `K=11` (split de
`D` au bit 0) ; la zone `10` appartient au grant de `J` mais XOR désigne `K`.

Test : `TestHOT1_GrantGuardNeedsNoView` (40 graines, 120 vnodes, bit choisi au
hasard au-dessus de la garde, aucune vue consultée).

### T2 — La frontière d'autorité est un CRDT d'état (`MAP-1`, `MAP-2`)

Énoncé. Soit `F(H)` la frontière d'un historique `H` clos par ancêtres : pour
chaque adresse, le grant terminal de sa chaîne et la hauteur de cette chaîne.
Alors `F(H1 ∪ H2) = F(H1) ⊔ F(H2)`, où `⊔` garde, adresse par adresse, le
grant de plus grande hauteur. `⊔` est commutatif, associatif et idempotent.

Argument. Un jeton a au plus un successeur (`AUTH-2`). Pour une adresse, deux
répliques détiennent donc deux préfixes de la même chaîne ; le plus long
contient l'autre. La hauteur ordonne totalement ces préfixes.

`MAP-2`. Deux successeurs d'un même jeton ont la même hauteur et des jetons
différents sur une adresse commune : `⊔` les refuse, dans les deux sens.

Conséquence. Routage et permission ne lisent que la frontière. Une réplique
peut échanger et conserver la frontière seule : O(feuilles), pas
O(certificats émis).

Ce qu'on perd. Un fork contre un ancêtre déjà élagué n'est plus détecté par le
receveur. Dans le modèle de pannes actuel (arrêt et reprise, certificats non
signés) ce fork ne peut venir que d'un bug : la source persiste la consommation
avant d'émettre. La garantie d'écrivain unique reste locale au disque.

Tests : `TestMAP1_FrontierJoinEqualsHistoryUnion` (300 histoires, trois
répliques partielles, comparaison exhaustive sur 256 adresses),
`TestMAP2_ForkIsRefusedByJoin`. Le modèle de référence est dans
`authority_map_test.go`.

### T3 — L'appartenance n'a pas besoin d'horloge causale

Constat. Les frontiers portées par les activations, retraits, placements et
intentions ne servent qu'à recalculer une cellule historique pour un contrôle
de couverture. Ce contrôle ne protège aucune écriture : l'autorité ne dépend
pas de l'appartenance. Son coût est O(N) par fait, donc O(N²) au total.

Proposition. La vue est une fonction de l'ensemble des faits, pas de leur ordre :

- activation valide ⇔ au moins une preuve, et chaque preuve désigne un grant ou
  une zone acquis par ce holder ;
- retrait prêt ⇔ ses preuves sont connues et le vnode ne détient plus aucune
  feuille (condition déjà vérifiée aujourd'hui) ;
- placement prêt ⇔ maillon précédent connu, preuves détenues par le nouveau
  disque, et l'ancien disque ne détient plus aucune feuille ;
- `V = {racine} ∪ activations valides − retraits prêts`.

Chaque prédicat ne dépend que des faits présents ; deux répliques ayant les
mêmes faits ont la même vue (`CRDT-2`). `CRDT-1` devient une union d'ensembles.

Ce qu'on abandonne. La clause d'`ACT-1` « chaque preuve touche la cellule ».
Elle ne peut pas être vérifiée sans contexte historique, puisqu'un ADD ultérieur
rétrécit la cellule. Un MOVE devient possible avant acquisition complète de la
cellule.

Statut : la proposition complète reste sur papier. Sa forme restreinte aux
activations et aux intentions est démontrée et livrée : voir E4a.

### T3a — Une activation n'a besoin que du contexte vide (`ACT-1`, `CRDT-2`)

Énoncé. Soit `V` une vue qui contient la racine `r` et le vnode `v`. Alors
`Cell(V, v) ⊆ Cell({r, v}, v)`. Toute pièce de preuve qui rencontre la vraie
cellule rencontre donc la cellule du contexte vide.

Argument. Une cellule est l'intersection des demi-espaces « premier bit
différent » contre chaque concurrent. Plus de concurrents donne plus de
contraintes, donc une cellule plus petite (`ADD-1`). Le publieur ne retient que
des pièces qui rencontrent sa cellule locale.

Conséquences. Le validateur actuel accepte déjà un contexte vide ; seul le
publieur change, et aucun format ne change. Les anciennes activations, avec
frontier, restent valides et donnent la même vue. La disponibilité d'une
activation ne dépend plus que de faits d'autorité, qui ne font que croître :
la vue est un point fixe unique, quel que soit l'ordre de livraison.

Ce qu'on perd. Une preuve dans `Cell({r, v})` mais hors de la vraie cellule est
acceptée. Le routage la corrige par forwarding, puis `Rebalance`. Aucune
permission d'écriture n'en dépend.

Intentions. Une intention dont tous les vnodes sont actifs ou retirés est
supprimée. C'est monotone, donc identique sur toutes les répliques.

Tests : `TestACT1_EmptyContextAcceptsEveryHonestProof` (2 000 vues aléatoires),
`TestCRDT2_LeanActivationsConvergeInAnyOrder` (80 histoires mêlant splits et
jointures à ID arbitraire, livrées en désordre, par lots, avec
reconstructions ; activations avec, sans ou mi-frontier : même vue, même
frontier, même autorité), `TestACT1_EmptyContextBoundary`,
`TestFulfilledIntentsAreDropped`, et les refus de résurrection de
`TestRetirementUsesCausalClockAndCannotResurrect`. Chaque test de convergence
et d'élagage a été vérifié en injectant un défaut : il échoue.

## 2. Architecture cible

Une seule structure partagée : la frontière d'autorité, un arbre binaire de
cubes dont chaque feuille porte `(hauteur, jeton, holder)`.

- Autorité : la feuille, plus l'installation locale (inchangé).
- Appartenance : les vnodes qui détiennent au moins une feuille.
- Placement : l'hôte et le disque du holder de la feuille.
- XOR : règle de planification. Il choisit l'adresse d'un nouveau vnode et la
  région à lui céder. À la quiescence, `OWN-1` est un théorème (T1).

Cette cible supprime activations, retraits, placements et intentions comme
faits propagés. Elle n'est atteignable qu'après E2 à E5 et la levée des
questions ouvertes de la section 4.

## 3. Étapes d'implémentation

Chaque étape est livrable seule. Critère commun : `go test -race ./...` vert,
aucun invariant de sûreté affaibli, lignes de production stables ou en baisse.

### E1 — fait

Index d'autorité et publication ordonnée dans `internal/causal`. Aucune
signature exportée ni format modifié.

### E2 — Ne plus reconstruire la réplique (`internal/storage`) — fait

Livré le 14 novembre 2026 sous une forme plus petite que prévu :
`CausalState.Replica()` garde les huit dernières répliques validées, indexées
par l'empreinte de `(racine, genèse, delta)`, et rend une copie en O(1)
(`TopologyReplica.Clone`). Les 18 appelants en profitent sans modification.
Obtenir la réplique coûte 0,7 ms, 13 ms et 180 ms à 64, 256 et 1 024 vnodes,
contre 4 ms, 56 ms et 0,9 s. Le reste est le hachage du bundle, quadratique en
taille tant que E4 n'est pas fait. Tests : `TestReplicaMemoHandsOutIndependentClones`,
`BenchmarkCausalStateReplica`.

Exigences d'origine, conservées pour référence :


- `MergeCausal`, et tout appelant de `CausalState.Replica()` sur un chemin
  chaud, partent de la réplique validée du cache (`causalKnowledge.replica`)
  et y fusionnent le delta. La réplique en cache n'est jamais mutée : `Merge`
  travaille déjà sur un brouillon.
- La reconstruction complète ne subsiste qu'à l'ouverture de la base.
- Le cache reste indexé par les octets exacts du contrôle. Aucune décision
  « writable » n'est réutilisée entre transactions.
- Acceptation : `BenchmarkTopologyRebuild` n'est plus sur le chemin d'une
  fusion ; un test compte les reconstructions pendant 100 fusions et en
  attend zéro.

### E3 — Garde lue sur le grant (`internal/protocol`, `indexus`)

- `PlanHotSplit` reçoit la région du grant terminal du donneur et calcule
  `g` par T1. `Guard(view, donor)` n'est plus utilisé pour décider.
- Précondition, vérifiée dans la transaction qui persiste l'intention : le
  donneur détient exactement un grant terminal, qui contient son ID, sans
  transfert entrant ni sortant en cours. Sinon : aucun spawn (`HOT-2`).
- `ValidatePlan` conserve la forme canonique à un bit.
- Acceptation : le scénario à concurrent omis ne capture plus aucune zone
  d'un tiers ; test de deux partitions qui splittent puis se rejoignent.

### E4a — Activations sans frontier, intentions élaguées — fait

Livré le 14 novembre 2026, sans changement de format :

- `mesh.publishActivation` n'envoie plus de frontier (T3a).
- `TopologyReplica.Merge` supprime les intentions accomplies.
- Le contrôle d'identité avant publication ne reconstruit plus la vue : les
  doublons sont déjà refusés à l'admission, il reste un ensemble d'IDs retirés.

| Vnodes | Connaissance avant | Après | Reconstruction avant E1 | Après E4a |
| --- | --- | --- | --- | --- |
| 64 | 121 Ko | 72 Ko | 6 ms | 0,7 ms |
| 256 | 1,1 Mo | 292 Ko | 285 ms | 4,9 ms |
| 1 024 | 14,4 Mo | 1,2 Mo | 15,9 s | 38 ms |

`CausalState.Replica()` (E2) suit : 0,4 ms, 1,2 ms et 8,4 ms. Garde-fou de
régression : `TestKnowledgeGrowsLinearly`.

Reste quadratique en principe : les retraits et les placements portent encore
une frontier, mais un par opération rare. L'historique des certificats croît
avec les handoffs, pas avec N : c'est l'objet d'E5.

### E4 — Appartenance sans horloge (`internal/causal`, puis appelants)

Reste à faire pour les retraits et les placements ; la partie activations et
intentions est couverte par E4a.

- Retirer `Dot`, `Frontier`, `Dependencies` et `dependencyView`. Appliquer les
  prédicats de T3.
- Avant le code : exploration d'états dans `internal/modelcheck`, 3 vnodes,
  toutes les permutations de livraison d'un historique avec ADD, MOVE et
  retrait. Attendu : même vue finale pour toute permutation ; aucune vue
  intermédiaire ne contient un vnode retiré dont le retrait est prêt.
- Vérifier que les intentions peuvent rester locales à leur auteur. Seul
  l'auteur les lit aujourd'hui (`PendingSpawnNames`).
- Le format persistant et le format du fil changent. Comme pour les transferts
  sans empreinte, les anciens faits sont refusés ; le dire dans la note de
  version. Vérifier l'usage de la frontier côté client (SDK).
- Acceptation : taille du bundle linéaire à 64, 256, 1 024 vnodes.

### E5 — Échanger la frontière, pas l'historique (`causal`, `storage`, `mesh`)

#### E5a — Transferts terminés élagués — fait

Livré le 14 novembre 2026 ; voir [handoff.md](handoff.md). Un transfert terminé
ne garde que son certificat et ses drapeaux ; un message par zone tardif est
ignoré au niveau du transfert. Sans ce garde-fou, le test
`TestCompletedTransferIsCompactedAndLateZoneMessagesAreNoOps` montre un
`install-zone` tardif qui remonte d'anciens octets par-dessus une écriture
acquittée.

| Mesure, campagne `scale` / `spike` | Avant | E5a |
| --- | --- | --- |
| Document de contrôle, total des nœuds | 4,0 / 4,5 Mo | 78 / 315 Ko |
| Plus gros nœud | 1 017 / 847 Ko | 20 / 41 Ko |
| États de zone de transferts terminés | 17 738 / 18 738 | 0 |
| Débit pendant les splits | dans le bruit (±8 %) | inchangé |

Essayé et écarté : garder le document décodé par version et donner des copies
profondes aux appelants. L'amorçage à 48 vnodes passe de 48–51 s à 21,5 s, mais
le débit pendant un pic recule de 18 % : sous charge chaque écriture crée une
version, le cache rate dans 97 % des cas et la copie s'ajoute au décodage.

#### E5b — La connaissance dans sa propre clé — fait

Livré le 15 novembre 2026 ; format décrit dans
[causal-topology.md](causal-topology.md#knowledge-key). À l'amorçage, le
document de contrôle était presque entièrement la connaissance (252 Ko sur 259
à 48 vnodes), réécrite et relue à chaque phase de handoff qui ne la change pas.
Elle a maintenant sa clé, réécrite seulement quand elle change ; le document de
contrôle garde son empreinte, si bien que les caches de permission, indexés par
ses octets, voient toujours un changement de connaissance.

| Mesure (deux runs alternés) | E5a | E5b |
| --- | --- | --- |
| Amorçage, 48 vnodes | 64,5 / 66,0 s | 26,0 / 25,9 s |
| Amorçage, 64 vnodes | 66–69 s | 37,3 s |
| Débit pendant un pic (`spike`) | 240 / 258 /s | 251 / 257 /s |
| Débit pendant les splits (`scale`) | 230 / 206 /s | 234 / 253 /s |
| Couverture de la cellule d'un nouveau vnode (`scale`) | 115–116 s | 91–98 s |

Contrairement au cache écarté plus haut, rien ne recule : sous charge la
connaissance change bien moins souvent que le document de contrôle, et son
cache réussit. Un ancien binaire ne lit pas le nouveau format.

#### E5c — Partenaires bornés (Kademlia) — première moitié faite

Livré le 15 novembre 2026, décrit dans [gossip.md](gossip.md) (`GOS-1`).
Chaque tour de maintenance parlait à tous les hôtes connus et à tous les
anciens donneurs : N·(N−1) échanges par tour. Il contacte maintenant un membre
par bucket XOR non vide, en tournant dans le bucket, plus un ancien hôte par
tour. Un fait traverse le réseau en au plus `profondeur` tours ; mesuré, 3 à 4
tours à 1 024 vnodes, avec 1 à 2,3 % des messages du maillage complet.

La carte complète reste : elle coûte 1,15 Ko par vnode et donne la route
directe sans recherche itérative. Ce qui reste à faire est le contenu :
un partenaire en retard reçoit encore le paquet entier. Le plan
(différence `V → W` vérifiée par reconstruction) est dans gossip.md.

Priorité mesurée le 14 novembre 2026 ([campagne](../archive/campaign-2026-11-14.md)) :
le décodage du document de contrôle `causal-v1` fait 67 % du CPU à l'amorçage
de 48 vnodes et 25 % pendant des splits sous charge. Il garde l'historique de
tous les transferts du nœud, terminés compris, et `CausalState()` le relit en
entier plusieurs fois par handoff. La première tâche d'E5 est donc de stocker
chaque transfert et chaque fait sous sa propre clé, et de ne garder des
transferts terminés que ce qu'exige la reprise.

- Passer le modèle de `authority_map_test.go` en production sous forme d'arbre
  binaire ; `⊔` en O(taille de la différence).
- Stocker une feuille par ligne de contrôle (zonelog, `internal/storage`). Le document unique `causal-v1` disparaît.
- Anti-entropie : comparer des empreintes de sous-arbres, n'envoyer que les
  feuilles différentes. Synchronisation périodique avec k voisins par
  distance XOR, plus les holders des transferts en cours.
- Les certificats restent la preuve échangée entre source et destination d'un
  handoff et dans leur état local de reprise. Ils ne sont plus propagés à tous.
- Acceptation : `MAP-1` et `MAP-2` sur la structure de production ; test
  différentiel contre le graphe de certificats ; convergence en O(log N) tours
  mesurée en simulation.

## 4. Questions ouvertes à trancher avant E5

1. `ZoneTransfer`. Une bascule par zone précède le certificat de région. Dans
   la frontière, c'est une surcharge portée par la feuille, indexée par
   `(collection, chemin)` car deux zones peuvent partager une adresse. Le
   certificat de région, de hauteur supérieure, l'efface. À modéliser.
2. Fusion de feuilles. Un retrait laisse au survivant plusieurs cubes. Pour
   garder O(N) feuilles il faut une opération locale qui consomme plusieurs
   jetons du même holder et sort un seul cube. Le graphe devient un DAG ; la
   hauteur du résultat est le maximum plus un. `MAP-1` est à revérifier.
3. Identité du vnode dans le holder. Dériver l'appartenance de la frontière
   suppose que le nom du vnode détermine son ID. Vrai pour les vnodes issus
   d'un split (`EncodeName`), à vérifier pour la racine.
4. Réplication. Elle change ce qu'est un holder (un ensemble de disques). À
   concevoir avant E5 pour ne pas figer un format à un seul disque.
5. `ZONE-2`. L'ordre enfants avant parents est-il requis pour l'écrivain
   unique, ou seulement pour les marqueurs d'enfants et les résumés ?

## 5. Règles pour l'implémentation

- Algorithme et invariants touchés d'abord, code ensuite.
- Test différentiel ancien/nouveau sur histoires aléatoires avant tout
  changement de structure ; il n'est pas commité.
- Aucun balayage de l'historique sur un chemin par écriture ou par fait.
- Ne jamais affaiblir : prédicat d'écriture unique rejoué dans la transaction,
  consommation persistée avant émission, fusion atomique sur brouillon,
  identité complète des zones, empreinte des données dans les transferts.
- Dire ce qui a été compilé et exécuté, et ce qui ne l'a pas été.
