# Configuration de production simplifiée

**Statut : actuel, en partie obsolète.** Les paragraphes sur le mapping mémoire et les pages Bolt ne s'appliquent plus : le moteur est zonelog. À réécrire. Voir [stockage-et-verrous.md](architecture/stockage-et-verrous.md).

Plan appliqué, testé et déployé le 7 novembre 2026. Le cœur causal distribué et
le format `indexus-flat-1` sont conservés. Aucune base ni clé n'a été remplacée.

## Avant / après

| Élément | Avant | Maintenant |
| --- | --- | --- |
| Nœud | 35 flags, variables d'environnement concurrentes | `-config`, `-version` ; JSON strict |
| Initialisation | `-createCluster` dans les unités de démarrage | Commande explicite `init` ; `serve` ne crée pas de genesis sans bootstrap |
| Déplacement | `-moveFrom` parmi les réglages quotidiens | `move -from URL -config FILE` |
| Découverte | `bootstrap` en `ip|port` et `topology` en URL | Une liste `network.bootstrap` d'origines HTTP(S) |
| Identité | Chemins indépendants des clés et certificats | Un dossier d'identité ; noms `node.ed25519` / `node.cert.json` |
| Authentification | Flag désactivé par défaut | Obligatoire dans le CLI ; aucun champ de désactivation |
| `delegation` | Lu et stocké sans consommateur runtime | Flag, env, champ, getter et argument de constructeur retirés |
| Compaction | 7 options de lancement | Active par défaut, `false` pour la couper ; seuils relatifs à la capacité (COLD-2) ; reprise durable conservée |
| HTTP/3 | Dépendances QUIC liées au nœud même sans TLS backend | HTTP/HTTPS natif dans le nœud ; HTTP/3 dans Caddy, le gateway autonome ou le nœud du module `quic/` (NET-1) |
| Profilage | Serveur pprof importé dans le nœud ordinaire | Build `go build -tags diagnostic ./cmd/node`, même application partagée |
| Issuer | 13 flags, certificats et provisionnement mélangés | 6 flags pour l'identité ; contrôleur AWS/local dans `cmd/provisioner` |
| Politique de pression | Struct et surcharges d'environnement dispersées | Valeurs internes et configuration explicite ; protections mémoire conservées |
| Panneau de lancement du dashboard | Réglages de protocoles retirés et de nombreux seuils | Taille maximale du lab et protection mémoire uniquement |

`cacheExpiration` inutilisée et les clients de downscale/IMDS non appelés ont
également été retirés. Le provisionneur conserve ses verrouillages de drain et
ses contrôles d'inventaire, hors du processus issuer.

## Une application, un format de configuration

Le point d'entrée de production est `cmd/node`. L'implémentation et ses tests
vivent dans `internal/app/node`, réutilisés par le build de diagnostic. Il n'y a
pas de fork mono-nœud du protocole ni de parser de compatibilité avec les anciens
flags. Les scripts, templates Terraform, unités systemd et lanceurs locaux ont
été adaptés. Les réglages non reconnus font échouer le démarrage.

Exemple : [ops/config/node.json](../ops/config/node.json).

```sh
node check -config /etc/indexus/node.json
node init -config /etc/indexus/node.json   # uniquement pour créer le cluster
node serve -config /etc/indexus/node.json # démarrage normal / reprise
node move -from http://old-host:21000 -config receiver.json
```

L'issuer doit être disponible pour émettre un certificat lors du premier
lancement, sauf si certificat et clé publique de confiance sont déjà fournis.
`init` n'est pas ajouté à l'ExecStartPre des services : une unité de production
ne doit pas réinitialiser automatiquement un disque vide.

| Groupe JSON | Informations |
| --- | --- |
| `storage` | `path` : préfixe de la base ; `identity_directory` : dossier des clés, par défaut parent du préfixe ; `memory_bytes` : budget mémoire du stockage, 256 Mio par défaut (16 Mio à 64 Gio) |
| `identity` | `network`, `cluster`, `issuer` ou `public_key` ; `name` pour une identité initiale explicite ; `prefer_near` : une cible, le nœud prend un nom neuf qui en garde 64 bits (SPAWN-1). Sans nom ni cible, un nœud qui rejoint demande `/join-hint` à son bootstrap (JOIN-1) |
| `network` | `listen`, `monitoring`, `advertise`, `bootstrap`, `tls_directory` |
| `limits` | `data_bytes`, `memory_refuse_pct` ; `writes_per_second` (CAP-2, expériences seulement) |
| `compaction` | Replie les paires de vnodes refroidies (COLD-2). Active si absent ; `false` la désactive |
| `retired_exit_seconds` | Si positif, le process s'arrête ce délai après le témoignage de la retraite de son vnode, pour libérer la place ; 0 garde le relais |
| `scaling` | `items_per_vnode`, `resource_signals` : réglages de l'autoscaler quand un contrôleur est configuré |
| `provisioner` | Origine du contrôleur optionnel ; absent sur la production actuelle |

Les chemins relatifs dépendent du dossier du fichier JSON, pas du répertoire de
travail. Les clés privées restent dans des fichiers séparés, jamais dans ce JSON.
L'identité du certificat persistant reste prioritaire pour conserver celle du
nœud au redémarrage. Le disque est toujours contrôlé par son manifeste causal.

`network` désigne le domaine d'authentification ; `cluster` l'identité causale
durable. La production conserve `indexus-dvf` et `indexus`, respectivement.
Ce ne sont pas des alias à fusionner.

Les listeners sont locaux par défaut. Le budget mémoire Go utilise toujours
`GOMEMLIMIT`, option standard du runtime. Les logs utilisent les réglages communs
de l'adaptateur de logging. Les anciens env de réglage de la politique de pression
ne remplacent plus les valeurs du nœud.

### Budget mémoire du nœud

La cible Go par défaut est le minimum de 75 % de la mémoire détectée et
1 536 Mio, y compris sur une machine dont la mémoire n'est pas détectable.
Un `GOMEMLIMIT` explicite reste prioritaire. Cette cible concerne la mémoire
gérée par Go ; ce n'est pas un plafond RSS. Le calcul de pression sous Linux
tient compte de la limite du cgroup du service, en plus de la RAM de l'hôte.

La protection distingue maintenant `rss_bytes`, `file_rss_bytes` et
`non_file_rss_bytes`. `mem_pct` retient le maximum entre la mémoire anonyme/
partagée rapportée au budget et la pression réelle de l'hôte calculée avec
`MemAvailable`. Les pages de fichiers mappés peuvent donc rester en cache
sans bloquer les écritures à tort. Le seuil configuré et son hystérésis restent
inchangés ; une pression réelle de l'hôte continue de déclencher la protection.
Cette distinction suit les [compteurs Linux](https://docs.kernel.org/filesystems/proc.html).

Les unités Linux générées réservent une marge avec `MemoryHigh=1792M` et
`MemoryMax=2G` (2 Gio). Le noyau peut récupérer des pages de fichiers et appliquer
de la pression ; une allocation non récupérable dépassant le plafond peut
déclencher un arrêt OOM. Ce mécanisme ne transforme pas arbitrairement les
objets Go en stockage disque. Un changement de ces templates n'applique pas
ces limites aux instances existantes sans mise à jour de leur unité.

### Mémoire du stockage

`storage.memory_bytes` est le budget du zonelog (`MemoryBytes`,
[storage/zonelog.md](storage/zonelog.md)) : un seizième pour chacune des deux
memtables, un huitième pour le cache de pages. Index et filtres des segments
passent par ce cache, comme les pages : ce qui sert reste en mémoire, le reste
est relu sur le disque. La part résidente fixe ne croît que de 0,03 o par
record (0,4 Mo à 15 M records) ; la RAM du stockage ne dépend donc plus du
nombre de records, seulement de ce budget. Mesuré à 15 M records et 256 Mio :
RSS 44 Mo après lectures chaudes, pic de 59 Mo pendant un import en bulk.

Pour un nœud qui porte une commune entière (11 à 15 M records, m6i.large,
`GOMEMLIMIT` 1 536 Mio) : `"memory_bytes": 536870912` (512 Mio). Le cache
passe à 64 Mo et contient les filtres de tous les segments (1,25 o par record,
19 Mo à 15 M) qu'une écriture consulte, plus les blocs d'index et les pages
chaudes ; RSS mesuré 80 Mo après lectures chaudes à 15 M. 256 Mio suffit à un
nœud surtout lu. Les memtables grandissent avec le budget (un seizième
chacune), et avec elles le log rejoué à l'ouverture.

Les données durables sont lues dans le zonelog ;
les inventaires réseau sont paginés (32 Mio maximum par RPC), les agrégats
projettent les métriques sans conserver les payloads, et la file d'écriture
retient au maximum 64 Mio de requêtes admises. Des métadonnées et agrégats
dérivés restent en RAM : le nœud n'est pas un moteur entièrement hors mémoire.
Les fichiers Bolt mappés sont paginés par le système ; leur espace virtuel
n'est pas la quantité de RAM réellement résidente.

Sur le Mac local, le lanceur utilise `GOMEMLIMIT=1536MiB`, mais systemd/cgroup
n'existe pas : il n'y a donc pas de garantie de plafond RSS strict de 2 Gio.
Une telle garantie locale nécessite un environnement Linux limité en mémoire.

## Séparation des responsabilités

- `internal/autoscale/resources.go` : mesures de ressources et plafond mémoire, indépendants
  du provisionnement. Pression non récupérable, cache de fichiers et budget Go
  restent des mesures distinctes.
- `internal/autoscale/autoscale.go` : décisions de pression et demande de capacité ;
  ne lance pas de processus AWS/local et ne donne pas d'autorité d'écriture. La
  façade (`indexus/autoscale.go`) ne garde que le branchement sur le nœud.
- `internal/adapter/issuer` : certificats, tokens et clé d'identité réseau.
- `internal/adapter/provisioner` : inventaire, création et terminaison de processus.
  Son serveur demande une autorisation ; l'exécutable vérifie un token réseau
  signé avec le scope `write`. Il ne détient pas la clé privée de l'issuer.
- `internal/adapter/http/p2p` : HTTP/HTTPS standard et authentification. Un verifier
  absent ne permet plus un accès lorsque l'authentification est requise.
- `gateway/http3server` : utilisé uniquement par le gateway de bord, dans son
  propre module Go (`gateway/go.mod`) : le module du nœud ne requiert pas quic-go.

La production actuelle ne lance pas de provisionneur supplémentaire. Les cinq
services existants restent les seuls actifs. Les mécanismes d'autorité, de
transfert, de journalisation, de reçus de retry, d'admission et de reprise ne sont
pas supprimés parce que cette instance est seule.

## Diagnostic explicite

```sh
go build -tags diagnostic -o /tmp/indexus-diagnostic ./cmd/node
# Après arrêt propre du nœud ordinaire :
INDEXUS_LOCAL_DEBUG_PORT=19031 /tmp/indexus-diagnostic serve -config node.json
```

L'outil ouvre les mêmes données, donc ne doit jamais tourner en parallèle avec
le nœud ordinaire sur la même base. Le profilage écoute exclusivement sur
127.0.0.1 et expose `/debug/memory`, `/debug/summary` et `/debug/pprof/`.
Le binaire de production ne lie ni ce serveur, ni `net/http/pprof`, ni QUIC.

## Taille et mesures

Même cible Linux amd64, Go 1.26.0 et CGO désactivé :

| Binaire nœud | Octets | Mo décimaux |
| --- | ---: | ---: |
| Ancien binaire déployé avec symboles | 15 894 822 | 15,895 |
| Ancienne source, build allégé comparable | 11 051 170 | 11,051 |
| Nouveau binaire de production | 8 773 794 | 8,774 |

Gain à options de build identiques : **20,6 %**. Gain face à l'ancien exécutable
réellement déployé : **44,8 %**, incluant le retrait des symboles.
L'issuer mesure 6 299 810 octets. L'outil de provisionnement est compilé séparément.
Le script commun `ops/deploy/scripts/build.sh` marque les exécutables avec leur
version et commit.

Il s'agit de taille disque/transfert, pas d'une baisse équivalente de RAM.
Le RSS comprend aussi les pages de données Bolt mappées et la phase de
réchauffement. Les buffers et protections du stockage n'ont pas été abaissés
pour produire artificiellement un gain mémoire.

Les relectures SDK de la racine DVF donnent 7 867 ms avant et 7 908 ms après
sur deux sessions neuves, un seul échantillon de chaque côté. Ce n'est pas un
benchmark de débit ni une garantie statistique. Cinq lectures HTTP directes à
chaud après déploiement prennent 20–22 ms, avec le bon total de 546 278 ventes.

## Vérification et déploiement

- Suite complète : 907 tests/sous-tests Go réussis, 9 ignorés, aucun échec.
- Tests de concurrence `-race` sur `indexus`, application nœud, P2P, mesh HTTP
  et provisionneur ; `go vet ./...` réussi.
- Nouveaux contrôles : JSON invalide/inconnu, absence de confiance, limites,
  découverte unique, commandes init/move, HTTP et TLS/HTTP2, refus d'accès
  sans verifier et arrêt du serveur.
- 66 tests dashboard, builds et validation Terraform réussis. Aucun apply
  Terraform ni changement d'instance AWS n'a été lancé.
- Local et production : quatre collections et cinq scènes IGN relues via SDK.
  Production : 564 837 records, services actifs, accès sans token refusé (401),
  écriture publique bloquée (404), scène Lyon affichée avec le statut « Prêt ».

La première tentative a déclenché un retour arrière automatique pour une
omission de la dépendance SDK dans l'assemblage du dashboard. L'assemblage a
été corrigé et vérifié par imports et démarrage sur port de contrôle avant la
seconde bascule réussie. Les données n'ont été ni réimportées ni remplacées.

Déploiement et retour arrière : la note de production AWS du 7 novembre 2026 (archivée hors du dépôt).
Les artefacts de vérification de cette intervention sont dans
`/tmp/indexus-simplification-20261107/`, hors des données `.local`.

### Croissance du fichier pendant les lectures

L'ouverture du stockage réserve un mapping virtuel dimensionné depuis le quota (deux fois le quota, plafonné à 64 Gio et à l'espace d'adressage de la plateforme). Cette réservation ne charge pas la base en RAM et ne préalloue pas ce volume sur disque ; Bolt conserve sa croissance physique habituelle. Elle évite les remappages fréquents qui attendent la fin des snapshots de lecture et bloquent les nouveaux lecteurs derrière un commit. Au-delà de la réservation, le mécanisme de croissance habituel reste actif. Les transactions et la durabilité ne changent pas.

Un test maintient un snapshot ouvert pendant une écriture de 20 Mio : le commit doit terminer avant la fermeture du snapshot et l'ancienne vue doit rester isolée.

Les projections de métriques lisent uniquement l'en-tête JSON canonique d'un `Record`. Le champ opaque `Payload` reste le dernier champ du schéma : sa validation est effectuée à l'écriture, son corps et son empreinte lors de la lecture du contenu. Les sommes ne nécessitent donc pas de parcourir les mégaoctets opaques de chaque bloc. Les tests comparent la projection au décodage complet (identifiants échappés, suppressions, métriques et contenus imbriqués) et verrouillent cet ordre des champs. Sur un enregistrement de 512 Kio, le microbenchmark local mesure environ 2,4 ms pour le décodage complet contre 2,4 µs pour l'en-tête ; ce n'est pas un facteur d'accélération global du serveur.

Les rescans périodiques des agrégats et le recomptage indicatif de monitoring attendent au moins une seconde sans écriture. Les deltas continuent de mettre les agrégats à jour pendant l'import. La préparation initiale, les changements d'autorité et les réparations explicites ne sont pas différés ; `Count()` explicite reste exact. `/status` et `/count` exposent leur dernière mesure indicative pendant un flux d'écritures, puis rattrapent au repos. Cette règle évite de retenir un ancien snapshot Bolt pendant un import continu, ce qui empêche la réutilisation des pages libérées et amplifie les écritures de freelist.
