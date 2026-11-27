# Vérifier les transferts sur le vrai code

> **Historique — remplacé le 25 novembre 2026.** Cette page conserve le
> protocole à preuves/checkpoints et ses outils antérieurs. Le runtime actif du
> worktree utilise [permissions locales et tickets](local-permissions.md) ; sa
> [qualification propre](local-permissions-verification.md) remplace les statuts
> d'implémentation ci-dessous, sans effacer ces décisions historiques.

**Statut : implémenté sur la branche authority-frontier, 25 novembre 2026.**

Le premier objectif est de trouver une erreur dans un scénario court, puis de
rejouer exactement ce scénario. Ajouter des outils ou annoncer un grand nombre
de simulations ne constitue pas une preuve. Le protocole reste en Go, sans
dépendance externe : le dépôt possède déjà un noyau d’autorité pur, des modèles
TLA+ et une interface de disque injectable.

## Trois niveaux complémentaires

| Niveau | Exécution | Ce qu’il vérifie |
| --- | --- | --- |
| Design borné | TLA+/TLC dans `spec/handoff` | Exclusivité, conservation des ACK et clôture sous les hypothèses de chaque modèle ; contre-exemples attendus |
| Transactions réelles | `internal/storage/handoff_schedule_test.go` | Vrais `Begin`, `Preload`, `Freeze`, `Stage`, `Emit`, `Install`, collecte, compaction et réouverture sur un disque à pannes |
| Concurrence et intégration | `go test -race`, tests mesh/façade, laboratoire HTTP | Accès concurrents réels, lectures/totaux et câblage du runtime ; pas de déterminisme promis pour ces tests |

Le harnais de transactions n’est pas une seconde implémentation de la décision
d’autorité. Il choisit les appels à effectuer et interroge `causalWritable`, le
vrai prédicat du disque. Son oracle de données garde la dernière valeur acquittée
de chaque registre, indépendamment des tables d’autorité. Des écritures ayant
renvoyé une erreur d’I/O peuvent néanmoins avoir persisté : leur résultat reste
incertain, il n’est pas assimilé à une annulation.

## Modèle de panne précis

Une seule graine commande les identités initiales, les choix de livraison et les
coupures. Les pertes de requête précèdent l’appel ; les pertes de réponse suivent
le commit réel. Les liens sont dirigés : une réponse peut être perdue alors que
la requête a été livrée. Les anciens messages rejoués proviennent de transferts
réellement effectués, sans inventer une commande d’installation jamais émise.

Le disque partagé de `internal/testkit/powerfs.go` vient des tests du moteur
zonelog. Il distingue écritures, synchronisation des fichiers et synchronisation
des noms de fichiers. Une coupure conserve les octets synchronisés et peut
laisser un suffixe non synchronisé déchiré. Le moteur réel décode ensuite ses
fichiers et son WAL ; aucune règle de récupération n’est réécrite dans le harnais.

Un défaut du simulateur existant a été reproduit puis corrigé : il consommait le
PRNG en parcourant une map de fichiers. Une même graine donnait donc des images
de disque différentes. Le parcours est désormais trié. L’allocation d’identité
dispose aussi d’une source d’entropie injectable, avec `crypto/rand.Reader` par
défaut et une erreur si cette source ne fournit pas assez d’octets. Une
réouverture conserve l’identité persistée et ne consulte pas cette source.

Les collecteurs sont attendus après chaque appel, les batches de données sont de
taille un et le budget mémoire évite les flushes concurrents de segments. Ainsi,
le harnais choisit les entrelacements **aux frontières des appels/transactions**.
Il n’explore pas tous les ordonnancements internes des goroutines. Les horloges
réelles n’y servent qu’aux observations du moteur, jamais à choisir une panne ou
la prochaine action. Les temps physiques et le débit ne sont pas simulés.

Le démarrage prépare de vraies régions vides et les admissions ACT-1 actuelles.
Les scénarios suivants déplacent les régions entre ces vnodes. Ils ne simulent
ni le provisionneur, ni l’autoscaler, ni l’activation du futur protocole à
membership seule. Les bit-flips d’octets déjà synchronisés, le byzantin et la
restauration d’une vieille sauvegarde sous la même identité sont hors du modèle.

## Invariants et vivacité

Après chaque action, pour chaque région représentée :

- au plus un disque en ligne passe le vrai prédicat d’écriture ;
- un écrivain disponible contient au moins la dernière valeur acquittée ;
- une valeur observée ne peut pas provenir d’une écriture jamais émise.

Après arrêt des pannes et des partitions, un drain équitable doit terminer les
transferts, laisser exactement un écrivain par région et ne garder aucune fence
ouverte. Les anciens ACK sont contrôlés **avant** toute nouvelle écriture de
remplacement. Une nouvelle compaction doit préserver ces propriétés. Une
campagne qui n’a exercé ni coupure d’I/O, ni réponse perdue, ni rejeu, ni écriture
chez le receveur avant clôture régionale est rejetée comme insuffisante.

Un test compare deux exécutions d’une même graine par le hash de leur trace
complète. Le fuzzing standard de Go fait varier une graine et un programme
d’actions ; sa réduction de corpus peut raccourcir un scénario qui échoue.

## Le blocage trouvé et corrigé

1. S émet la cession ; R installe durablement.
2. L’ACK d’installation est perdu.
3. R coalesce ce droit avec son voisin puis compacte les reçus terminés.
4. S réessaie l’installation. L’ancien code exigeait encore un token actif et
   refusait le retry : S ne pouvait plus terminer, même sans panne restante.

Le contexte de consommation de l’autorité permet une réponse idempotente plus
simple. Une ancienne opération reconnue comme obsolète est acquittée **sans
remonter ses données et sans installer de droit**. Le contrôle lie cluster,
source et incarnation destinataire. Le prédicat autorisant les écritures reste
inchangé. Aucun journal supplémentaire d’installations n’est ajouté.

Cet argument utilise le contrat de pairs honnêtes et de checkpoints acceptés :
consommer le droit d’un disque par une nouvelle cession ou une coalescence
suppose son acquisition préalable. Un message reconnu obsolète ne constitue
pas une preuve cryptographique contre un pair malveillant. Le donneur ne
collecte qu’un transfert qu’il a lui-même émis durablement.

`TestInstalledReplySurvivesLocalCoalescingAndReceiptCompaction` échoue avant le
correctif. Après celui-ci, il vérifie aussi que l’ACK répété ne change aucun
octet de contrôle/connaissance et qu’un autre cluster, une autre source ou une
autre incarnation destinataire ne bénéficie pas de ce raccourci.

`InstallReceipt.tla` conserve le contexte de consommation après la coalescence
et la compaction. Sous équité faible et livraison possible, la clôture finit.
`ForgottenInstallReceipt.cfg` conserve l’ancien refus et doit trouver un
contre-exemple de vivacité. Ce modèle ciblé a 11 états distincts ; il n’est pas
une preuve de raffinement de toutes les transactions Go.

## Qualification du 25 novembre 2026

La qualification complète passe sur `82cda13` avec un arbre de travail propre :
`go test ./...`, `go vet ./...`, `go test -race ./...`, 2 737 exécutions de
fuzzing en 30 secondes, deux scénarios à 64 disques et les 14 contrôles TLC
(6 modèles positifs, 8 contre-exemples attendus).

Les deux scénarios à 64 disques donnent la même trace SHA-256 :
`706b6cad8b433b8b826e23c2551840b78b6fd93427bb9d2a9f548a15840068ef`.
Chacun termine 49 transferts après 166 crashes/reprises, 74 réponses perdues,
9 coupures d’I/O et 173 rejeux. Il exerce 19 écritures chez le receveur avant
la fin régionale. Ces nombres décrivent cette graine, pas une probabilité de
panne ni une mesure de performances.

La première passe complète s’était arrêtée sur une course dans la map de
partition du réseau **de test**, ainsi qu’une garde de cinq secondes dépassée
sous instrumentation. Le harnais utilise désormais son verrou existant pour
modifier une partition. La garde utilise `RaceScale`, déjà employé dans le
dépôt ; les assertions et le blocage volontaire des constructeurs Knowledge
sont conservés. Les deux tests passent aussi cinq fois ensemble sous `-race`.
Les logs de cette première passe restent conservés séparément des résultats
verts ; ce n’était pas une qualification réussie.

Le blocage de reçu décrit plus haut concerne la **nouvelle compaction de cette
branche**. Il ne démontre pas la cause des anciennes défaillances AWS. Le mode
Knowledge réduit à la membership, les annonces ciblées et la migration d’un
annuaire régional restent à implémenter et à qualifier séparément. Les
[successeurs de lecture locaux](read-delegations.md) constituent l’étape suivante
implémentée ; leur qualification est distincte de ce résultat historique.

## Reproduction

```bash
# Même graine, deux traces identiques ; 32 graines par défaut dans la suite.
INDEXUS_HANDOFF_SEED=25 go test ./internal/storage -run TestHandoffScheduleReplaysSeed -count=1 -v
INDEXUS_HANDOFF_SEEDS=128 go test ./internal/storage -run TestHandoffScheduleSeeds -count=1 -v

# Vrais 64 disques simulés, sans HTTP ni machines AWS.
INDEXUS_HANDOFF_64=1 INDEXUS_HANDOFF_TRACE=/tmp/handoff-trace.json \
  go test ./internal/storage -run TestHandoffSchedule64 -count=2 -v

# Le corpus qui échoue est conservé et réduit par Go.
go test ./internal/storage -run '^$' -fuzz '^FuzzHandoffSchedule$' -fuzztime=30s -parallel=2

# Qualification locale ; aucun provisioning ni commande AWS.
bash scripts/research/check-handoff.sh --quick
TLA2TOOLS_JAR=/path/to/tla2tools.jar bash scripts/research/check-handoff.sh --full
```

`INDEXUS_HANDOFF_RESULTS` choisit le dossier des logs du script. Un échec arrête
la qualification et laisse ses logs. Le mode complet exécute la suite Go, vet,
la suite entière sous `-race`, le fuzzing, deux passes à 64 disques et les
18 contrôles TLC (7 modèles positifs et 11 contrôles négatifs). Le mode rapide ne se présente pas comme ce mode complet.

Le choix méthodologique suit les descriptions primaires de
[FoundationDB](https://github.com/apple/foundationdb/blob/main/documentation/sphinx/source/testing.rst)
et du [VOPR TigerBeetle](https://github.com/tigerbeetle/tigerbeetle/blob/main/docs/internals/vopr.md) :
exécuter le code réel en contrôlant ses sources de non-déterminisme. Ces projets
ne constituent pas une preuve des résultats Indexus. `testing/synctest` peut
aider les tests d’horloges/goroutines ; il ne remplace pas ce contrôle du disque
et des livraisons. Porcupine/Elle seront pertinents pour des historiques dont le
contrat de cohérence aura été précisément fixé, sans supposer la linéarisabilité
de tous les résumés éventuellement convergents.

Voir [frontière d’autorité](authority-frontier.md),
[modèles bornés](../../spec/handoff/README.md) et
[tests de lecture en transition](read-write-phase-verification.md).
