# Qualification locale entre processus

Ce banc complète le laboratoire Go partagé : un agent réel lance cinq processus
vnode Linux, un issuer et un loader HTTP direct. Quatre joins arrivent ensemble,
six secondes après le début des écritures. Chaque variante repart de disques
vides. Aucun appel AWS, aucun changement de seuil ni accord d'autorité simulé.
Le récepteur `/v1/scale` journalise les demandes et répond explicitement 503 :
**il ne qualifie ni la fusion des demandes du provisioner, ni le placement**.

Construire les quatre binaires pour l'architecture de l'image Linux choisie :

```sh
mkdir -p /absolute/bin
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /absolute/bin/node ./cmd/node
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /absolute/bin/issuer ./cmd/issuer
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /absolute/bin/experiment ./cmd/experiment
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /absolute/bin/probe ./ops/experiment/multiprocess/probe.go
python3 ops/experiment/multiprocess/run.py --bin /absolute/bin \
  --out /absolute/results/copy1-gmp2 --image YOUR_LOCAL_LINUX_IMAGE \
  --copy-slots 1 --gomaxprocs 2
python3 ops/experiment/multiprocess/analyze.py /absolute/results
go test -count=1 -v ./internal/storage -run '^TestInventorySinceSkipsWhatACommitDidNotTouch$' > /absolute/results/since-work.log
node ops/experiment/sizing/calibrate-queues.mjs /absolute/results/summary.json /absolute/results/calibration.json
```

L'image doit avoir `/bin/sh`, `mkdir` et `sleep`. Les binaires sont fournis par
un montage en lecture seule. Les ports localhost 33800–33802, 33810–33814 et
33900–33904 doivent être libres. Le script conserve `/data` puis supprime
uniquement son propre conteneur, même en cas d'échec. Ne réutiliser ni le nom
ni le répertoire d'un essai ; l'échec d'un audit interdit la variante suivante.
Les clés de l'issuer restent dans les résultats locaux, jamais dans le dépôt.

Comparer `--copy-slots 1/2` et `--gomaxprocs 1/2/default` : 100 écritures/s,
35 secondes, huit collections, un lecteur à dix lectures/s dont 20 % de totaux.
`GOMEMLIMIT=512MiB` par processus est une limite souple du runtime, pas un quota
mémoire de machine ; le stockage garde sa configuration de production.

`--stress` ajoute une charge CPU contrôlée de six goroutines pendant 40 secondes
et porte l'essai à 70 secondes. Il exige une VM Docker à huit cœurs, sans quota
CPU ajouté par le banc. `/proc/stat` couvre la **VM Linux entière**, donc aussi
les autres services Docker éventuels. Cet essai est volontairement dégradé et
ne sert pas à comparer le p99 normal. Il ne mesure pas le CPU total de macOS.

`--quiet-seconds 35` observe les totaux non rafraîchis sur chaque nœud après
l'arrêt des écritures, avant l'audit exact, pour distinguer convergence des
résumés, cache de comptage et conservation des ACK. Ces observations ne sont
pas remplacées par des lectures exactes silencieusement.

## Ce que les mesures permettent de dire

- Les histogrammes périodiques donnent des bornes supérieures de buckets
  logarithmiques, pas des percentiles exacts. Leur nombre de mesures peut être
  inférieur au total final d'ACK (dernière fraction non publiée).
- Les timestamps d'observation ont une résolution d'environ une seconde.
  `taken` clôt une opération ; il ne date pas le premier droit installé.
- `disk.by_kind` distingue attente et tenue du writer par opération. La tenue
  comprend calcul, fsync et ordonnancement : ne pas la compter une seconde fois
  comme du CPU ou de l'I/O dans le modèle.
- Le budget refuse désormais une nouvelle copie non gelée après dix secondes
  sans jeton ; receveur et reprise restent sans attente. Sous pression CPU,
  la source cède du temps entre les lots déjà basculés. Les compteurs et les opérations observées ne donnent pas le
  maximum exact de copies simultanées. Le test séparé
  `TestCopyBudgetSeparateProcessesAndCrash` qualifie les flocks réels, le partage
  d'un même transfert et la libération par mort du processus, pour un/deux jetons.
- Un passage à petite charge ne choisit pas le meilleur GOMAXPROCS et ne prouve
  ni 16k/s, ni la distribution sur plusieurs machines, ni l'absence de régression
  par rapport à une ancienne version.

Le lecteur de totaux du loader suit désormais le champ `contact` lorsque `set`
est nul. Une boucle ou une route absente donne une erreur explicite ; elle ne
se transforme plus en un total de zéro. Le TLS de l’entrée est conservé.
Les anciens `totals_deficit` ne permettent donc pas à eux seuls de mesurer le
retard des résumés : ils incluaient aussi des redirections ignorées.

## Provisioner et réserve sur un réseau de conteneurs

`placement.py` utilise le vrai provisioner, les vrais hooks `facts.sh`,
`count.sh`, `spawn.sh`, et deux agents avec cinq slots chacun. Un troisième
conteneur héberge l’issuer, le provisioner et le loader. Les interfaces réseau
et les pools de slots sont distincts ; **le CPU physique et `/proc/stat`
restent ceux d’une seule VM Docker**.

Construire également `provisioner` et reconstruire `node` depuis l’arbre testé :

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /absolute/bin/provisioner ./cmd/provisioner
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /absolute/bin/node ./cmd/node
python3 ops/experiment/multiprocess/placement.py --bin /absolute/bin \
  --out /absolute/results/placement-demand --image YOUR_LOCAL_LINUX_IMAGE \
  --trigger demand
```

**Mode demand :** 400
écritures/s pendant 45 s, moitié sous le préfixe `Hot` mais avec des zones
feuilles distinctes ; signal de demande configuré à 150 écritures/s/vnode avant
le démarrage. C’est une configuration de banc, pas une nouvelle valeur par
défaut. Le CPU reste ciblé à 60 %, les autres seuils de production sont conservés.
La demande doit venir du contrôleur du vnode, garder `Wanted()` vrai pendant le
join, lancer un receveur sur la réserve et lui faire appliquer des écritures.
Le banc échoue si cette capacité reste inutile, si un processus sort ou si
toutes les arrivées ne sont pas acquittées ; il termine par l’audit exact.

`--trigger controlled` envoie cinq demandes `host_pressure` au provisioner.
Cela vérifie la coalescence de l’orchestrateur **sans rendre les donneurs
chauds**. Le premier essai a bien donné un spawn et quatre demandes fusionnées,
puis échoué : le donneur refusait le transfert avec `ErrNotNeeded`. Ne pas
relire ce succès de lancement comme une preuve de capacité utile. Le garde
`Wanted()` n’a pas été désactivé pour faire passer le banc.

Après un échec de qualification, les conteneurs et le réseau de cet essai sont
supprimés ; les fichiers restent dans les outputs. Aucun essai suivant ni appel
AWS n’est lancé automatiquement. Le passage suivant doit être lancé explicitement après analyse du précédent. Les ports localhost utilisés sont 34800–34803,
34900–34904 et 34910–34914.

La première exécution du mode demand est décrite dans
[le rapport du banc](../../../docs/benchmarks/demand-qualification-2026-11-26.md).
Elle a bien fait appliquer du trafic aux receveurs avant la fin des joins,
mais l’audit initial a rencontré 503 pendant le dernier transfert. L’audit
corrigé attend explicitement les transferts/joins et distingue `total_errors`
des différences numériques. Le [passage suivant](../../../docs/benchmarks/settled-demand-2026-11-26.md)
sur `0b4ddb4` réussit : 17 999 ACK relus, 17 totaux exacts concordants,
après 5,84 s d’attente observée de l’audit. Cela qualifie ce scénario local,
pas plusieurs machines physiques ni AWS. Les compteurs consultatifs d’items
restent surestimés et le CPU partagé dépasse ponctuellement 60 % après la charge.

Le [passage sur `36e2b31`](../../../docs/benchmarks/claude-copies-2026-11-26.md)
échoue ensuite sur `inventory changed during paginated read` pour un total,
malgré les 17 999 ACK relus. Aucun retry de total ni relance de qualification
n’est ajouté pour rendre cet essai vert.

```sh
python3 ops/experiment/multiprocess/replay.py /absolute/captured-run /absolute/replay
INDEXUS_INVENTORY_REPLAY=/absolute/captured-run go test -count=1 -v ./internal/mesh -run '^TestReplayCapturedInventory$'
```

Le replay de stores est opt-in, sur **copies temporaires**, et vise la fixture
`Placeh0000000000` de 8998 records de cet incident. Il ne prouve ni absence de
course pendant un transfert, ni réussite du banc physique. Le replay HTML
réutilise `live.py` et les composants du dashboard ; les deux pools restent
étiquetés comme partageant une seule VM.

La correction des curseurs et les politiques de copies/chaleur sont décrites
avec leurs limites dans [la qualification suivante](../../../docs/benchmarks/elastic-anticipation-2026-11-26.md).
L'audit propage maintenant l'annulation à ses requêtes et publie `incomplete`
et `canceled` lorsqu'il est interrompu après le démarrage des lectures. Il
compte les lectures achevées, saute la seconde relecture après un contrôle
complet et borne à 32 workers le diagnostic des collections discordantes
après un contrôle échantillonné. Un désaccord reste un échec.
