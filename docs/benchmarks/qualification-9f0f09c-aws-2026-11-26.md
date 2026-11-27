# Qualification AWS de 9f0f09c — 26 novembre 2026

- **Arrêt sur un défaut de transport confirmé : les contacts de voisinage parlent HTTP à des nœuds HTTPS.**
- Les cinq passages du labo 64 nœuds passent, dont deux répétitions et une avec fautes sur la nouvelle version.
- Sur le cluster réel H3, 26 000 écritures sont acquittées et retrouvées ; les lectures concurrentes observées sont propres.
- Un vrai profil CPU de receveur est récupéré et décodé ; le placement complet et la montée 16k/s restent non qualifiés.
- Les quatre instances créées sont terminated ; réseau, bucket et rôles supprimés ; estimation EC2 haute 0,486 USD, hors services annexes.

## Périmètre et provenance

Source figée : `9f0f09c111e9301dea5fb9c8e3345250eb3b3b8d`, arbre
`f09066d7842567776cacf89ef488b0e928066ba2`. Aucun correctif de protocole,
changement d'assertion ou modification de `claude/next` pendant cette passe.
Le [rapport local](qualification-9f0f09c-2026-11-26.md) garde les résultats
antérieurs : suites normales, H3 et race detector vertes ; performance du
portable écartée à cause du swap et placement empêché par le disque.

Campagne `qual-9f-1126-122347`, région eu-west-3. Accord explicite de 2 USD,
TTL 45 minutes, deux i4i.xlarge initiaux avec deux slots chacun, plafond de
trois hôtes, un c6i.4xlarge de mesure. Préflight frais : quota 512 vCPU,
autre usage 2 vCPU, aucune expérience active ; prix EC2 relevés par l'API
AWS : 0,398 et 0,808 USD/h. Les machines étrangères à la campagne sont
laissées intactes. Le loader reste figé à `2362b35` ; agent, node, issuer
et provisioner proviennent de `9f0f09c`.

L'instrumentation est archivée hors du dépôt : canal SSM sur le rôle de cette
campagne, commandes de mesure et collecte, bootstrap du loader mis en attente
au lieu de lancer un scénario de charge automatiquement. Le rôle temporaire
SSM a été détaché et supprimé à la fin. Les binaires de production restent
ceux de la source figée ; le nœud H3 est construit avec `-tags diagnostic`.
Les configurations de charge et de compaction sont indiquées ci-dessous.

Les résultats compacts, empreintes des binaires et mesures sont conservés dans
[le relevé JSON](qualification-9f0f09c-aws-2026-11-26.json). Les mesures locales
macOS, la simulation HTTP sous Linux et le cluster H3 réel sont des bancs
différents : leurs temps ne sont pas regroupés en un pourcentage de gain.

## Labo 64 nœuds — mesuré sur AWS, une machine physique

Même machine c6i.4xlarge, passages séquentiels, GOMAXPROCS=8, 64 services/stores
et 32 dispositifs fsync simulés, HTTP loopback, 8 192 zones initiales,
32 Mio par store, 40 écritures/s au total, quatre phases de 10 s, joins
pendant la charge puis traîne au même débit. Fsync et maintenance actifs.
Comptabilité incrémentale de benchmark explicite ; les tests unitaires locaux
avaient gardé leur audit exhaustif. Aucun processus de cluster réel ne tourne
pendant ces comparaisons.

| Source / passage | ACK | p50 ms | p95 ms | p99 ms | Dernier join, s | Allocations Gio | CPU hôte inactif min, % |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 2362b35 / before-2362 | 1600 | 9.82 | 35.74 | 70.14 | 36.72 | 29.87 | 58.0 |
| 1afbd3d / before-1af | 1600 | 9.81 | 26.57 | 64.80 | 39.49 | 16.36 | 68.7 |
| 9f0f09c / new-r1 | 1657 | 9.81 | 24.73 | 55.86 | 41.44 | 17.65 | 71.9 |
| 9f0f09c / new-r2 | 1632 | 9.86 | 23.51 | 50.09 | 40.81 | 17.12 | 70.8 |
| 9f0f09c / new-faults | 1684 | 9.80 | 27.95 | 70.34 | 42.12 | 17.48 | 71.5 |

Les cinq audits terminent sans ACK manquant ou dupliqué, sans doublon d'ACK
chez les détenteurs écrivables et sans gel restant. Le passage avec fautes
injecte notamment une perte de réponse après installation durable, une partition
et une traîne d'I/O ; les erreurs de tentative et les reprises restent dans
les traces, même lorsque l'écriture finit par réussir.

Aucun swap sur les cinq passages ; le CPU hôte reste sous 70 %. Les percentiles
de ce tableau viennent des traces individuelles, pas des classes log2.
Les allocations sont cumulées et échantillonnées, préparation et audit compris :
ce ne sont ni le RSS ni des allocations par écriture. Le nombre d'ACK diffère
parce que la traîne continue jusqu'à la fin des joins. Le JSON fournit aussi
les phases initiales communes de 40 s pour éviter cette confusion.

**Ce que cela confirme :** sur ce petit banc, les deux passages sans faute de
la nouvelle version ont un p99 observé plus bas ; les allocations sont nettement
plus faibles que sur 2362b35. **Ce que cela ne confirme pas :** une fermeture
plus rapide des joins, un gain statistique universel ou la capacité 16k/s.
Les références n'ont qu'un passage chacune. La dernière fermeture est légèrement
plus tardive sur la nouvelle version. Rien ici ne prouve un p99 indépendant de N
ou du volume de données.

## Cluster réel — ce qui a effectivement tourné

Deux agents sur deux hôtes NVMe, issuer et provisioner réels, nœuds H3 avec
monitoring TLS, clients de charge TLS/TCP depuis le loader. Le scénario final
charge 8 000 écritures à 400/s, puis 18 000 à 200/s. Seuil réel de 2 500 items
par vnode, signaux de ressources désactivés pour déclencher sur ce seuil,
compaction froide désactivée pour isoler le placement. Les appels de scale-up
sont produits par les nœuds, pas fabriqués par le scénario final.

Un premier essai de commande du harnais a oublié son jeton de provisioner et
reçu 401 ; seul le harnais a été corrigé. Un essai manuel à froid a ensuite
reçu `donor no longer needs a peer` : c'est le refus attendu d'un join de scaling
sans pression, pas une perte d'écriture. Le receveur est sorti avec code 0.
Les disques et journaux de ce scénario ont été archivés avant le scénario
à pression réelle. Il ne faut pas additionner leurs ACK aux 26 000 du scénario
final ni présenter le refus à froid comme une régression du protocole.

| Assertion ou observation | Résultat du scénario final |
| --- | --- |
| Écritures offertes / ACK / shed / épuisées | 26 000 / 26 000 / 0 / 0 |
| Audit final, toutes les clés ACK relues | 26 000 vérifiées, 0 manquante, 0 erreur de lecture |
| Totaux profonds par collection à l'audit | 16 collections, pas d'écart |
| Zones détenues deux fois à l'audit calme | 0 ; aucun transfert encore ouvert à cet audit |
| Lectures ponctuelles concurrentes du loader | 1 545, 0 clé manquante, 0 erreur signalée |
| Totaux profonds pendant la charge | 607, tous au moins égaux aux ACK antérieurs connus du lecteur |
| Lectures additionnelles de clés de préparation déjà ACK | 1 104, aucune manquante ni indisponible |
| Ressources des agents observées | CPU des processus nœuds sommés : maximum 40,43 % de l'hôte ; mémoire libre min 97,29 %, disque libre min 99,28 % |

Les compteurs périodiques du loader ne comprennent pas nécessairement la
fraction finale de seconde. Ses journaux donnent des **bornes de classes** :
p99 ACK de 8,192 ms en préparation et 16,384 ms en phase live ; p99 depuis
l'arrivée prévue de classe 16,384 ms sur les deux phases. Ces bornes ne sont
pas les quantiles exacts du tableau du labo. Cette charge est très inférieure
à celle des anciens décrochages AWS : aucune conclusion de capacité 16k/s.

Le receveur `JtybsiSKCSMYq6D0` est observé client-ready et encore joining pendant
36 échantillons, sur environ 3,92 s avant la première observation joining=false.
Le monitoring est donc réellement disponible avant la fin du join. Son premier
compteur d'ACK HTTP positif est observé environ 0,43 s après la première métrique,
encore avant la fin du join. Ce compteur seul ne distingue pas une écriture
appliquée localement d'une réponse relayée ; le premier ACK direct au détenteur
n'est pas établi par ce compteur.

Le statut 503 avant activation est observé dans l'essai de join refusé à froid.
Le scénario chargé a déjà un receveur actif au premier échantillon : la fenêtre
préactivation y est manquée, et ne devient pas une preuve supplémentaire.
L'audit calme ne prouve pas, à lui seul, l'unicité d'écrivain à chaque instant
sur les vrais processus. Cette garantie passe dans le labo et ses assertions,
mais une preuve temporelle exhaustive du cluster réel reste hors couverture.

### Profil CPU et writer du receveur

Le relais agent `/profile?slot=1&seconds=1` retourne 200, les stacks retournent
200 et le profil est décodé avec le binaire H3 exact archivé. Durée une seconde,
560 ms d'échantillons CPU : ce n'est pas un profil vide d'un faux nœud. Les
métriques joining et les faits avant/après la capture sont conservés. Une seule
fenêtre ne permet pas de classer les goulots d'une campagne de plusieurs minutes.

Les fenêtres disponibles de dix secondes donnent, pour la tenue du writer par
tentative du receveur : control 1,53–3,76 ms, mount 6,47–8,08 ms, collect 5,35 ms.
Les nombres de tentatives, commits durables et attentes sont dans le JSON.
Ces moyennes ne sont pas un temps de fsync, un maximum de gel ou la preuve d'un
coût plat sur des transferts longs. On ne les compare pas directement aux
140–300 ms des anciens essais sous plusieurs milliers d'écritures/s.

## Défaut confirmé — le transport de voisinage ignore TLS

**Mesure réelle et lecture de code concordantes.** Après le join, les logs
répètent `client sent an HTTP request to an HTTPS server`. Pour chacun des deux
nœuds actifs, la même requête de ping munie d'un jeton valide donne :

| URL | Réponse |
| --- | --- |
| `http://IP:PORT/ping` | 400, `Client sent an HTTP request to an HTTPS server.` |
| `https://IP:PORT/ping` | 200 et identité correcte du contact |

Reproduction sans altérer les droits : obtenir un token read du vrai issuer,
envoyer `{"origin":{"ips":{},"port":0}}` avec Authorization Bearer aux deux
URL du même nœud. Le ping HTTPS avec client token ne fournit pas une fausse
identité de pair. Les corps de réponse et horodatages de campagne sont archivés.

Dans `internal/adapter/peer/contact.go`, `Contact.ping` construit littéralement
`http://.../ping` ; `Contact.endpoint` construit également `http://...` pour les
lectures de découverte. Le `HttpClient` de ce paquet est un transport TCP brut,
sans sélection de schéma depuis l'endpoint TLS ni adaptation meshhttp/H3.
`Node.Observe` utilise ces contacts pour enregistrer et sonder les voisins.
Le transport de transfert mesh peut fonctionner pendant que ce chemin de
voisinage échoue : un join réussi ne valide donc pas tout le réseau.

Ce chemin est préexistant au lot de onze commits ; la passe ne l'attribue pas
à 9f0f09c. La correction devra utiliser le schéma et le transport effectifs,
avec un test de processus TLS couvrant ping, découverte et reprises. Aucun
correctif de production n'est ajouté dans cette qualification.

## Autre incohérence à reproduire — cible de spawn et identité créée

**Mesuré :** la cible demandée est `JtybsiSKCSMnKK7w`, le vnode actif créé est
`JtybsiSKCSMYq6D0`. Le donneur redemande ensuite la même cible, alors qu'à l'audit
les deux nœuds détiennent respectivement 12 817 et 13 183 items, au-dessus du
seuil de 2 500, et qu'un autre hôte est vide. Le scénario reste à deux nœuds
actifs pendant sa fenêtre de mesure. Une instance de réserve supplémentaire
démarre à 12:41:08 UTC, près de l'expiration des trois minutes du filtre de
cible du provisioner, pendant l'arrêt. Le journal de décision correspondant
n'est pas capturé : son type de placement n'est pas déclaré mesuré.

**Lu dans le code :** `preferNearsForDemand` renvoie les PendingSpawnNames avant
de réévaluer un plan ; `PendingSpawnNames` et `prunePendingSpawns` retirent une
réservation par égalité exacte du nom avec les membres connus. Le mécanisme de
spawn choisit pourtant une nouvelle identité proche de la cible. Une intention
réservée sous X et un membre installé sous Y ne se correspondent donc pas par
cette égalité. Le provisioner conserve aussi asked[X] pendant targetPatience,
trois minutes, après un lancement accepté.

**Interprétation forte, pas mesure directe du disque pending-spawns :** cette
incohérence peut recycler la même intention et retarder l'offre de capacité.
Les traces seules ne démontrent pas tout le contenu du registre durable ni
l'ensemble des causes de la cadence ; le défaut TLS ci-dessus existe aussi.
La reprise doit reproduire l'intention avec le vrai issuer, puis relier son
identifiant à l'identité effective par un reçu durable. La réservation doit
se terminer à l'étape prévue du transfert, sans rendre un droit sur timeout.

## Pourquoi le labo ne détectait pas cela

Le labo HTTP boucle sur des services/façades ; il n'exécute pas toute la chaîne
binaire H3 → contacts historiques → issuer qui fabrique une identité proche →
provisioner. Ses noms de vnodes sont préparés. Il teste les reprises et la
sûreté des transferts ; il ne couvre pas le schéma TLS des contacts ni la
correspondance complète entre une cible planifiée et un nom réellement émis.
Un modèle de sûreté ne fournit pas automatiquement cette preuve de vivacité.
Ces deux scénarios doivent devenir des régressions d'intégration avant de
reprendre la montée en charge.

## Ce qui reste non qualifié

- Placement local appliqué sur faits réels. Le type de décision associé à la
  réserve démarrée tardivement n'est pas capturé ; new, existing et fallback
  cap ne sont donc pas déclarés qualifiés de bout en bout.
- Pas de preuve d'activation complète et de migration sur la machine de réserve,
  démarrée juste avant l'arrêt et immédiatement terminée avec le reste.
- Pas de gros payloads par zone pour qualifier le budget adaptatif en octets.
- Pas de comparaison H3 réelle avant/après à 16k/s, pas de conclusion de coût
  par transaction plat sur une longue montée en charge.
- Pas de preuve exhaustive d'écrivain unique temporel sur les processus réels
  à partir des seuls snapshots `/ownership` ; leur audit final est propre.

Le compteur de membership reste observable dans les traces, sans être renommé
Knowledge globale. La présence d'un état de membership compact ne dispense pas
de mesurer sa fréquence d'envoi. Cette passe ne développe pas d'optimisation
supplémentaire de sa diffusion.

## Arrêt, coût et preuves

Les quatre instances de cette campagne sont indépendamment vérifiées terminated,
y compris une réserve démarrée à 12:41:08 UTC pendant la clôture. Bucket S3, rôle,
profil IAM, launch template et ses versions, security group, subnet et VPC sont
vérifiés supprimés. Aucun autre service de l'utilisateur n'a été arrêté.

Estimation EC2 haute jusqu'à l'observation indépendante de terminaison :
**0,486 USD**, minimum de facturation d'une minute inclus pour la réserve.
Ce n'est pas un relevé de facture et cela exclut EBS, S3 et transfert. Le
plafond autorisé est resté à 2 USD ; aucun second test 16k/s n'a été lancé.

Traces brutes et harnais de campagne dans le workspace opérateur :
`outputs/qualification-9f0f09c-20261126/aws/`. Ce dossier contient les cinq
journaux du labo, ACK, profils d'allocations, profils CPU et stacks des vrais
receveurs, snapshots de monitoring, reproduction HTTP/HTTPS, manifestes de
source et de binaires, instrumentation, preuves de suppression et calcul de coût.
Le rapport automatique générique de run.sh n'interprète pas ce format de
qualification : ses lignes « 0 nodes » ne sont pas le résultat de ces audits.
