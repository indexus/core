# Propriétés mathématiques et informatiques d'Indexus

> **Synthèse historique — contexte du 25 novembre 2026.** Les fondements
> géométriques et algébriques sont conservés. Les affirmations d'implémentation,
> le CRDT du graphe d'autorité (§2.1), les chaînes de certificats et les mesures
> ci-dessous décrivent leurs révisions d'origine, pas le nouveau runtime.
> [Permissions locales](../protocol/local-permissions.md) décrit le remplacement
> effectif ; sa [qualification](../protocol/local-permissions-verification.md)
> n'hérite d'aucun ancien résultat AWS. Les expressions historiques « production »
> ou « livré » ne constituent pas une vérification de déploiement actuel.

**Statut : actuel.** Propriétés mathématiques ; les mesures citées ont leur date.

Référence historique de la première synthèse (`281e15d` dans `core`, `76c3644` dans `sdk-js`, `f914647` dans `dashboard`, `340d563` dans `experiences`, `1976d34` dans `landing`).

Indexus est un index spatial/temporel distribué qui projette tout espace multidimensionnel dans **un seul arbre binaire** sans coordinateur central ni annuaire global. Ce document rassemble ses fondements mathématiques, ses propriétés de convergence (CRDT), son algèbre d'agrégation composable, la définition auto-certifiante des collections (`IXC1`), son plan de contrôle P2P et les résultats mesurés en conditions réelles.

---

## 1. L'objet informatique : un espace unique et trois plans séparés

### 1.1 Espace d'adressage `{0,1}^96` et métrique XOR (`ID-1`, `ZONE-ID-1`, `OWN-1`)

- **Un seul espace 96 bits** : toute zone et tout vnode vit dans $\{0,1\}^{96}$ (12 octets, soit 16 caractères dans l'alphabet `0-9A-Za-z-_` à 6 bits par caractère).
- **Adresse d'une zone** : `ID(z) = bits(location) || bits(collection)` complété à 96 bits. Une `location` est préfixe de tous ses descendants.
- **Identité canonique vs adresse de routage** (`ZONE-ID-1`) : l'identité d'une zone est le couple `(collection, location)`. Deux zones (un parent et un descendant coïncident) peuvent partager la même adresse 96 bits tout en conservant des enregistrements, barrières et certificats d'autorité distincts.
- **Propriété purement géométrique** (`OWN-1`, `TOPO-1`) :

$$\mathrm{Owner}(z, V_{\mathrm{active}}) = \arg\min_{v \in V_{\mathrm{active}}} \bigl(\mathrm{ID}(z) \oplus \mathrm{ID}(v)\bigr)$$

La fonction d'appartenance ne dépend ni de la capacité disque, ni de la chaleur d'écriture, ni d'un annuaire : deux nœuds ayant le même ensemble actif $V_{\mathrm{active}}$ désignent exactement le même propriétaire pour toute zone.

### 1.2 Séparation stricte en trois objets (`docs/protocol/query.md`)

| Objet | Rôle | Peut autoriser une écriture ? |
| --- | --- | --- |
| **Vue logique** | Arbre parcouru par le client (`frontier`, `visited`, `bounds`) | Non (un marqueur d'enfant indique une existence, pas un propriétaire) |
| **Topologie de routage** | Ensemble local $V_{\mathrm{active}}$ donnant le prédicteur XOR | Non (`ROUTE-1` : une vue périmée provoque un relais ou un refus, jamais un écrivain parallèle) |
| **Autorité d'écriture** | Grants durables et chaînes de certificats sur disque (`AUTH-1..4`) | Oui (grant non consommé, barrière régionale ouverte, vnode actif, bon placement physique) |

En architecture logicielle (`internal/architecture`), le noyau est découpé en 7 couches unidirectionnelles : `tree → authority → store → wire → walk → gpu → bords`.

---

## 2. Convergence et propriétés CRDT

Indexus sépare le **plan de contrôle** du **plan de données répliqué**. La première synthèse décrivait ce dernier comme simulé seulement. Depuis le jalon `d689156` du 22 novembre, les shadows, journaux de fallback, lectures dégradées et réparations sont câblés dans le stockage et le mesh, avec des tests locaux de partition et de redémarrage. Cela ne requalifie pas les campagnes AWS historiques en essais de réplication distante.

### 2.1 Plan de contrôle : la frontière d'autorité est un CRDT d'état (`T2` / `MAP-1`, `MAP-2`)

Chaque jeton d'autorité a au plus un certificat successeur (`AUTH-2`, persisté avant émission et conservé au redémarrage par `AUTH-4`). Pour toute adresse, deux répliques détiennent donc deux préfixes d'une même chaîne causale.

En notant $F(H)$ la frontière d'un historique $H$ (pour chaque adresse : son grant terminal et sa hauteur) :

$$F(H_1 \cup H_2) = F(H_1) \sqcup F(H_2)$$

où $\sqcup$ conserve, adresse par adresse, le grant de plus grande hauteur (`MAP-1`).

- **Lois algébriques** : $\sqcup$ est **associatif, commutatif et idempotent** (demi-treillis supérieur).
- **Détection de fork (`MAP-2`)** : deux successeurs distincts d'un même jeton ont la même hauteur et des jetons différents sur une adresse commune ; $\sqcup$ les refuse symétriquement.
- **Borne mémoire** : la frontière dérivée a une taille en $O(\text{feuilles})$. Cette borne ne porte pas sur tout l'historique de preuves conservé ni sur le paquet Knowledge complet, qui peut grandir avec les joins.

### 2.2 Convergence de la vue d'appartenance (`CRDT-1`, `CRDT-2`, `T3a` / `ACT-1`, `T1` / `HOT-1`)

- **Activation sans contexte historique (`T3a`)** : pour tout vnode $v$ et toute vue $V$ contenant la racine $r$ et $v$, $\mathrm{Cell}(V, v) \subseteq \mathrm{Cell}(\{r, v\}, v)$ car ajouter des concurrents ne fait que rétrécir une cellule XOR (`ADD-1`). Toute preuve qui touche la vraie cellule touche donc déjà la cellule du contexte vide.
- **Convergence éventuelle (`CRDT-2`)** : lorsque les mutations de topologie cessent et que les deltas manquants arrivent, toutes les répliques convergent vers le même $V_{\mathrm{active}}$ quel que soit l'ordre de réception des messages (les intentions dont tous les vnodes sont activés ou retirés sont éliminées à la fusion).
- **Garde de hot-split locale (`T1` / `HOT-1`)** : si $D$ détient un grant terminal $R$ dont le bit contraint le plus profond est $g - 1$, tout split $J = D \oplus e_b$ avec $b \ge g$ cède $R \wedge (\text{bit } b = J[b])$ sans modifier la cellule XOR d'aucun autre vnode. Deux splits sur des grants disjoints **commutent** et n'ont pas besoin de consulter la vue globale.

### 2.3 Plan de données : réplication convergente (`internal/replication/convergent`)

Par défaut sur `main`, la réplication est désactivée : chaque zone possède au plus un écrivain autorisé (`AUTH-1`), ce qui ne suffit pas, à lui seul, à garantir la fraîcheur des lectures. Une zone devient indisponible si son seul disque manque ; une lecture rafraîchie doit couvrir ses détenteurs nécessaires ou échouer, tandis que les résumés restent dérivés.

Le module `internal/replication/convergent` relâche cette contrainte en conservant la convergence déterministe :

- **Ensemble de répliques** : $R(z)$ désigne les 3 vnodes actifs les plus proches de $z$ selon XOR sur des machines distinctes ($R(z)[0]$ est le propriétaire).
- **Estampillage** : chaque écriture porte un identifiant unique `dot = (incarnation, counter)` et une horloge de Lamport locale à la zone `ver`, accompagnés d'un vecteur de versions par incarnation.
- **Règle de fusion (ordre total sans horloge physique)** : pour un identifiant d'enregistrement donné, entre deux versions, gagne dans l'ordre :
  1. le plus grand `ver` ;
  2. à `ver` égal, la version marquée supprimée (le tombstone l'emporte) ;
  3. à égalité, le plus petit digest de payload.
- **Propriétés** : cette relation d'ordre total rend la fusion **associative, commutative et idempotente** (CRDT de type *LWW-element-set* causal).
- **Trois modes locaux** :
  - *Sain* : un seul écrivain ($R(z)[0]$), réplication vers les shadows sur $R(z)[1..2]$. Le chemin peut attendre une confirmation de réplica durable, puis se rabattre sur la durabilité locale après timeout : tout ACK ne prouve donc pas deux copies durables. Les résumés reçus peuvent rester en retard.
  - *Fallback* (sous partition après silence $T_{\text{suspect}}$) : les répliques joignables acceptent les écritures dans un journal de fallback et servent des lectures marquées `degraded`.
  - *Réparation & élagage* : au retour du propriétaire, les écritures de fallback repassent par le chemin normal d'application ; les tombstones ne sont purgés qu'une fois la zone `clean` et le vecteur de versions couvert par tous les membres de $R(z)$.

---

## 3. Agrégation hiérarchique, composabilité et opérations par niveau

### 3.1 Algèbre des colonnes (`internal/algebra`)

Historiquement, toute colonne d'un résumé de zone était une somme compensée de Neumaier (`wire.Abelian`, groupe abélien). Comme le protocole de propagation remplace la contribution versionnée de chaque enfant au lieu d'appliquer des deltas non versionnés (`docs/protocol/query.md`), **tout monoïde associatif avec élément neutre converge** :

| Régime | Condition sur la loi | Ce qui converge | Compromis |
| --- | --- | --- | --- |
| Demi-treillis (`min`, `max`, union) | idempotente et commutative | les valeurs elles-mêmes, sans versions | plusieurs écrivains possibles |
| Deltas de groupe (`+= Δ`) | groupe commutatif | seulement si chaque delta arrive exactement une fois | fragile : interdit par `AGG-1` |
| **Remplacement versionné (Indexus)** | **tout monoïde, même ordonné** | **les registres par enfant `(version, valeur)`** | **écrivain unique par zone, versions croissantes** |

`internal/algebra` définit ainsi un `Schema` par colonne (jusqu'à **4 096 colonnes** via les pages larges `IXR2`/`ZLS4`, `IXRB v3`, `IXB2` kind 4, avec encodage `float32` sans perte lorsque toutes les valeurs sont des `float32` exacts) :

| Type (`Kind`) | Structure algébrique | Commutatif | Inversible | Comportement au retrait / mise à jour |
| --- | --- | :---: | :---: | --- |
| `Sum` | Groupe abélien (somme de Neumaier) | Oui | Oui | Delta $O(1)$ sans rescan |
| `Min`, `Max` | Demi-treillis | Oui | Non | Rescan de la zone uniquement si l'élément retiré portait l'extrême (mesuré : $7{,}19$ rescans pour $2\,000$ retraits, conforme à $H_n - 1 = 7{,}18$) |
| `First`, `Last` | Monoïde ordonné | Non | Non | Combiné strictement dans l'ordre des cellules |
| `Affine` `(a, b)` | Monoïde ordonné non commutatif | Non | Non | Composition $(a_1, b_1) \circ (a_2, b_2) = (a_1 a_2,\; a_2 b_1 + b_2)$, calculant toute récurrence linéaire $h \leftarrow a h + b$ (EMA, filtres linéaires, intérêts composés, états SSM) sur n'importe quel préfixe (FMA interdit pour garantir le même arrondi Go `arm64` ↔ JS) |

### 3.2 Indépendance de coupe et composabilité (`Cut Independence`)

Pour toute zone $P$ :

$$\mathrm{Summary}(P) = \mathrm{Direct}(P) \;\circ\; \bigodot_{c \in \mathrm{Children}(P)} \mathrm{Summary}(c)$$

combinée en **ordre de cellule** (`ChildRank`, soit l'ordre des bits avant rotation, puis par `id` d'enregistrement). Grâce à l'associativité de $\circ$ et à cet ordre canonique :

1. **Indépendance de coupe** : toute coupe complète de l'arbre lue dans l'ordre des adresses produit exactement la même valeur que la racine (vérifié sur 1 000 dispositions aléatoires avec stockage creux à toutes profondeurs).
2. **Composabilité client (`walk`)** : un lecteur peut stopper sa descente à n'importe quelle profondeur dès que les bornes ou la résolution lui suffisent ; une lecture sur plage $[u, v]$ se couvre par au plus $2(2^c - 1)$ zones par niveau ($2^c - 1$ pour un préfixe).
3. **Composabilité topologique (`split` / `fold`)** : le partitionnement XOR peut couper l'arbre à n'importe quel niveau sans altérer les agrégats ; à la bascule ou au changement de vue, les vnodes réconcilient les contributions via les numéros de séquence de zone sans jamais compter deux fois un enregistrement (`internal/summary/store.go`, `typed.go`).
4. **Composabilité inter-collections & statistiques suffisantes** : deux collections partageant la même empreinte d'espace (`indexus.space.v1`) ont la même cellule pour le même lieu et peuvent être jointes cellule par cellule malgré des rotations ou schémas distincts ; toute statistique issue de sommes ($\Sigma x$, $\Sigma x^2$, centroïdes, $X^\top X$, $X^\top y$, sketches de Fourier) se compose exactement.
5. **Coût CPU mesuré** (`internal/algebra`, 8 colonnes, conteneur 2 vCPU) : `41 ns` (sommes), `55 ns` (mixte somme/min/max), `229 ns` (4 paires affines).

### 3.3 Opérations par niveau : choix v1 et morphisme de monoïdes

Dans la spécification actuelle (`IXC1`, `Descriptor.Schema`), **le schéma d'agrégation est identique à toutes les profondeurs** (`RecordDepths` borne seulement `[lo, hi]`) :

- **`d < lo`** : aucune écriture autorisée ; lecture de l'agrégat `S(z)` et des clés `/sets` des enfants ; les colonnes ordonnées suivent l'ordre d'entrelacement $\sigma$ (type courbe de Morton).
- **`lo ≤ d ≤ hi`** : insertion, remplacement et suppression (tombstone) de records ; si $\sigma$ ne coupe plus qu'un seul axe $a$ sous $c \cdot d$ (`OrderedAxis`), `first`/`last`/`affine` suivent l'ordre naturel de cet axe (par exemple une série temporelle par lieu).

Avec une transition $\varphi$ entre tranches de profondeur, seules les coupes alignées sur les frontières de tranche resteraient valides et $\varphi$ devrait être un **morphisme de monoïdes** ($\varphi(x \oplus_1 y) = \varphi(x) \oplus_2 \varphi(y)$), vérifié par une projection de colonnes mais faux en général pour une fonction apprise ; c'est pourquoi v1 garde un monoïde unique.

---

## 4. Définition mathématique des collections (`IXC1`)

Dans `internal/algebra` (`descriptor.go`, `space.go`, `name.go`), une collection est un objet binaire canonique purement mathématique (32 à 185 octets en test, sans nom de colonne ni unité métier) :

| Champ | Rôle mathématique |
| --- | --- |
| `BitsPerChar`, `Width` | Géométrie des adresses (base $2^c$, longueur $W$ caractères, $N = c \cdot W$ bits) |
| `Nonce` (16 octets) | Sépare deux collections partageant exactement la même algèbre |
| `RecordDepths [lo, hi]` | Plage de profondeurs autorisée pour les enregistrements |
| `Space` | $k$ axes bornés $[\min_a, \max_a]$ ($\Delta_a = \max_a - \min_a$) et cédule explicite $\sigma$ indiquant quel axe est coupé par chaque bit |
| `Schema` | Type d'agrégation (`Sum`, `Min`, `Max`, `First`, `Last`, `AffineA`/`AffineB`) de chaque colonne |

### 4.1 Quantisation dyadique exacte et bissection IEEE 754

Après $n$ coupes sur l'axe $a$, une coordonnée $x$ tombe dans la cellule dyadique :

$$j_a(x, n) = \operatorname{clamp}\!\left(\left\lceil \frac{2^n (x - \min_a)}{\Delta_a} \right\rceil - 1,\; 0,\; 2^n - 1\right)$$

La bissection flottante en IEEE 754 `binary64` coïncide **bit pour bit** avec cette quantisation exacte tant que toutes les frontières restent représentables sans arrondi, soit jusqu'au **niveau dyadique** $D_a$ vérifiant :

$$\max(|\min_a|, |\max_a|) \le 2^{53 + \min\bigl(v_2(\min_a),\; v_2(\Delta_a) - n\bigr)}$$

où $v_2$ est la valuation 2-adique ($v_2(0) = +\infty$). `IXC1` refuse toute description dépassant $D_a$ : ainsi, l'adresse d'un point est un objet mathématique identique sur Go, navigateur/JS et GPU.

### 4.2 Rotation bijective et auto-certification par le nom

- **Rotation** : le caractère au niveau $l$ est $A[(b_l + \mathrm{rot}(l)) \bmod 2^c]$ où $\mathrm{rot}(l)$ dépend de `name[l mod 16]`. Cette bijection par niveau disperse deux collections couvrant le même espace physique sur des propriétaires XOR différents tout en préservant la structure de l'arbre.
- **Nom auto-certifiant et espace opaque (`k = 0`)** :

$$\mathrm{name} = \text{96 premiers bits de } \mathrm{SHA\text{-}256}\bigl(\texttt{"indexus.collection.v1\textbackslash0"} \,\|\, \mathrm{IXC1}\bigr)$$

encodés en 16 caractères de l'alphabet de routage (empreinte complète 256 bits conservée par les nœuds ; empreinte d'espace `SHA-256("indexus.space.v1\0" || axes || masque)` pour la compatibilité géométrique). Avec $k = 0$ axe (espace opaque), l'écrivain choisit directement chaque caractère (cas du routage par centroïdes SPFresh/LIRE) avec la même algèbre d'agrégation. Tout changement de borne, de cédule, d'opération ou de nonce produit un nouveau nom et de nouvelles clés de zones (`DESC-1`..`DESC-5`).

---

## 5. Plan de contrôle P2P : gossip Kademlia, propagation et transfert granulaire

### 5.1 Gossip borné par préfixes (`GOS-1`, `internal/mesh/gossip.go`)

Chaque nœud conserve la carte de connaissance (~1,15 à 2,4 Kio/vnode). Pour éviter le coût quadratique $N(N-1)$ d'un échange tous-à-tous :

- Les détenteurs actifs sont regroupés dans au plus 97 seaux selon la longueur du préfixe commun avec l'ID du nœud local.
- À chaque tour, le nœud contacte un pair par seau non vide (avec rotation interne et rotation des anciens détenteurs/donneurs de certificats).
- **Propriété `GOS-1`** : pour tout couple $(s, t)$, le pair contacté par $s$ dans le seau de $t$ est $t$ lui-même ou partage un préfixe strictement plus long avec $t$. Un fait atteint donc tout nœud en $O(\log N)$ échanges par tour et au plus `depth` tours (mesuré : 3–4 tours sur 1 024 vnodes aléatoires, 3 tours sur un trie déséquilibré « spike »).

### 5.2 Agrégation P2P ascendante et bornée

Les écritures acquittées plient localement les deltas dans la zone puis poussent de manière asynchrone et coalescée (au plus 1 tentative/s par arête enfant→parent, par lots d'au plus 512 contributions) vers le propriétaire XOR du parent. Le tick périodique `Update` répare les arêtes après tout changement de topologie.

### 5.3 Élasticité par transfert granulaire (`FLIP-UNIT`, `internal/mesh/flip_units.go`, `handoff_stream.go`)

Plutôt que de geler toute une région pendant la copie complète :

- Le transfert découpe l'ensemble en **lots adaptatifs, initialement 16 zones** (de 4 à 2 048 sans surcharge, cible 1 Mio, plafond 8 Mio/zone), ordonnés **profondeur décroissante, puis chaleur à profondeur égale**.
- Chaque lot est copié par lots de snapshots (`Store.Snapshots` en une seule transaction `s.view`), rattrapé via `Store.ZoneSeqs`, certifié et installé durablement sur le receveur, qui devient **écrivable une fois l’installation et l’activation acquises** pour ce lot tandis que le reste de la région continue d'être servi par le donneur.
- La clôture régionale parcourt les états par pages de 512 zones (`eachZoneStatePaged`) avec vérification $O(1)$ par compteur de version, réduisant les longs scans monolithiques de cette version historique. Le stockage actuel utilise des snapshots MVCC ; cela ne prouve pas une borne universelle de gel. La cible de gel par lot est de 40 ms (réduction au-delà de 80 ms), et l'attente locale de 500 ms ne borne qu'une attente, pas l'ACK complet.

---

## 6. Synthèse des benchmarks mesurés (`main`)

Toutes les campagnes AWS ci-dessous ont tourné sur `eu-west-3` avec des nœuds `i4i.xlarge` dédiés (1 vnode/machine, NVMe local, `fsync` actif, HTTP/3 sur QUIC + TLS, chargeurs `c6i.4xlarge`).

### 6.1 Passage à l'échelle horizontal (passes fixes et rampe jusqu'au genou)

| Protocole | Taille | Débit vert soutenu | Par vnode | Latence p99 | Audit durabilité |
| --- | --- | ---: | ---: | ---: | --- |
| `MODE=passes` (3 passes) | 16 vnodes | 47 991 /s (CV 0,0 %) | 3 000 /s | ≤ 524 ms | PASS, 0 perte, 0 double |
| `MODE=passes` (3 passes) | 32 vnodes | 95 987 /s (CV 0,0 %) | 3 000 /s | ≤ 524 ms | PASS, 0 perte, 0 double |
| `MODE=passes` (passes 1 & 3) | 64 vnodes | **190 700 /s** (`99,3 %` eff.) | 2 980 /s | ≤ 524 ms | PASS (passe 2 rouge sur layout déséquilibré REM-120, durable) |
| `MODE=ramp` (`exp-20261121-165008`) | 4 vnodes | 14 492 /s | **3 623 /s** | **262 ms** (intent.) | PASS (1,63 M clés, 0 / 0) |
| `MODE=ramp` (`exp-20261121-165008` / `174708`) | 8 vnodes | 28 902 – 33 024 /s | **3 613 – 4 128 /s** | 262 – 524 ms | PASS (3,10 M clés, 0 / 0) |
| `MODE=ramp` (`exp-20261121-174708`) | 16 vnodes | **57 679 /s** (pic 73 295 /s) | **3 605 /s** | **262 ms** (intent.) | PASS (5,99 M clés, 0 / 0) |

- **Linéarité 4 → 16 vnodes (rampe)** : **99,5 %** à latence p99 strictement plate (`262 ms`), équilibrage `owned_bytes = 1,00×` et `writes_received = 1,00×`, `0` seconde de désaccord.

### 6.2 Élasticité live et handoff granulaire

| Expérience | Profil | Résultat clé | Durée / latence handoff | Audit |
| --- | --- | --- | --- | --- |
| Local `handoff.py` (`512 Mio / 65 536 zones`, HTTP + `fsync`) | 1 donneur → 1 receveur | 1er ACK receveur en **2,38 s** (vs `51,89 s` avant) | Pause lot : **63,5 ms** moy. / **85,0 ms** p95 ; ACK client max `272,7 ms` | PASS (67 100 ACKs) |
| AWS `exp-20261121-171716` (`RAMP_ELASTIC=1`, `d=120s`) | 4 → 13 vnodes live | `9 568 /s`, 3/3 doublements en **0,0 s** | 8 splits live réussis (0 erreur) | PASS (1,41 M, 0 / 0) |
| AWS `exp-20261121-173647` (`RAMP_ELASTIC=1`, `d=60s`) | 4 → 8 vnodes live | **22 386 /s**, 4/4 doublements en **0,0 s** | 4 splits live en **26,1–40,6 s** | PASS (1,91 M, 0 / 0) |
| AWS `exp-20261121-180233` (`RAMP_SHARE=0`, sauts ×2 `d=60s`) | 4 → 11 vnodes live | **16 004 /s**, `p99 = 65,5 ms`, 4/4 paliers en **0,0 s** | 8 splits live en **7,5–13,0 s** | PASS (1,38 M, 0 / 0) |

---

## 7. Limites, non-garanties et état sur `main`

### 7.1 Ce qui n'est pas garanti (`docs/protocol/consistency.md`, `failure-model.md`)

- **Pas de délai borné de convergence**, ni pour la topologie ni pour les agrégats (sous partition, aucune borne ; la guérison converge comme un message en retard).
- **Pas d'instantané cohérent inter-zones** : lire un parent puis ses enfants peut chevaucher un handoff ; les résumés utilisés pour élaguer une branche sont des bornes indicatives (`refresh=true` recalcule depuis les inventaires autoritaires).
- **Lectures pendant un transfert (`READ-1`)** : toute écriture acquittée avant le début de la lecture est visible, sinon un shard injoignable renvoie une erreur (jamais un résultat partiel silencieux).
- **Disponibilité & modèle de fautes** : sans réplication câblée, un intervalle sans écrivain est possible pendant un gel et la perte d'un disque entraîne la perte de son jeton. Le modèle suppose des participants honnêtes (crash-recovery, certificats non signés).

### 7.2 État d'avancement sur `main`

- **Livré et validé en production/AWS** : espace `{0,1}^96`, autorité causale CRDT (`MAP-1`/`MAP-2`, `ACT-1`, `HOT-1`), gossip `GOS-1`, transfert granulaire `FLIP-UNIT` + clôture paginée, définition mathématique `IXC1` et colonnes typées jusqu'à 4 096 colonnes côté `core` (`internal/algebra`, `internal/summary/typed.go`).
- **En cours / partiel** :
  - `sdk-js` possède localement (`76c3644`, 2 commits devant `origin/main`) le support des descriptions/colonnes typées et des lignes > 255 métriques ; `IXB2` section 4 et les index de segments pour grandes zones typées restent à finaliser.
  - `internal/replication/convergent` est validé par simulation déterministe et `internal/mesh/replicas.go` calcule déjà `R(z)`, mais le câblage `storage`/`mesh` (copie ombre, fallback, réparation) reste à réaliser.
  - `E5c` (envoi de différences de connaissance plutôt que le paquet entier) et `T3` complet restent à finaliser.
- **Non encore éprouvé à grande échelle sur AWS** :
  - Lectures concurrentes pendant des splits élastiques sous rampe (`-readers` avec `-scenario ramp`).
  - Splits sur donneurs pré-chargés à fort volume (512 Mio–1 Gio+) avec hotspots Zipf.
  - Pannes/kills de nœuds en plein milieu d'un handoff et rampe élastique à 32–64 vnodes.
  - Compaction/élagage long terme de l'historique `ZoneProof`/`ZoneTransfer` après des centaines de cycles split/fold.

## Révision des garanties — 24 novembre 2026

Le code de référence inclut `d5e9873` (Knowledge découplé), `031fa1a`
(erreurs de lecture transactionnelles), `7e7ee41` (intégration du nettoyage)
et `6a3015e` (inventaire et receveurs dans un snapshot). Le correctif
SUM-4 de Claude est intégré dans `b2d3dea`. Les anciens identifiants en tête restent
la provenance de la synthèse, pas une validation actuelle de tous les dépôts.

Deux courses distinctes étaient possibles : calculer un total avec une
somme locale avant collecte et une soustraction après collecte (SUM-4),
ou lire l'ancienne copie du donneur puis perdre sa liste de receveurs à la
fin du transfert (READ-1). Les deux corrections rendent cohérents les
éléments locaux qui doivent être lus ensemble. Elles ne donnent pas un
snapshot global ni une borne de propagation des résumés.

Le chemin d'écriture compact ne reconstruit pas Knowledge sur un cache froid
et ne fusionne pas synchroniquement les métadonnées facultatives de la
réponse. Les échanges en arrière-plan sont bornés et dédupliqués ; les
paquets complets restent utilisés pour le contrôle. Une écriture vérifie
toujours son autorité durable. Le lot installé et activé peut écrire avant
la synchronisation finale du join, mais le détour par le donneur peut
persister jusqu'à l'apprentissage de la route. La mise à jour locale du
24 novembre ajoute cet indice exact au nœud d’entrée (WRITE-HINT-1), avec
un cache partagé de 4 096 zones et validation d’autorité toujours sur le
disque visé ; elle est intégrée dans `bfb8432`.

La passe intégrée a validé `go test ./...`, `go vet ./...` et dix répétitions
avec `-race` des tests de total bulk, de collecte intercalée et de lectures
concurrentes. Le nouveau test de lecture échoue avec l'ancien code dans
deux scénarios déterministes et réussit après correction. Les anciens
échecs intermittents n'ont pas été reproduits lors des répétitions initiales :
leur attribution exclusive à cette course n'est pas établie. Ce sont des
validations locales, sans nouveaux chiffres AWS.

Voir [le registre des garanties](../protocol/invariants.md),
[leurs tests et limites](../protocol/verification.md),
[le transfert actuel](../architecture/transfert-de-zone.md) et
[Knowledge](../architecture/knowledge.md).
