# Indexus — qualification locale du protocole simplifié

Qualification locale terminée sur le code de `e65dfd3`. Cette campagne ne constitue pas une mesure de capacité AWS ni une preuve générale du code distribué.

## Code et périmètre

Branche : `codex/authority-frontier-20261124`. Binaire de qualification : `e65dfd3`.
Le dépôt principal sur `main` reste séparé. Aucun essai AWS ni nouvelle ressource payante n'a été lancé.

Les trois agents ont travaillé sur le stockage/permissions, le transport/transfert, et les garanties/lectures. Le principal a intégré les façades, les métriques et les laboratoires.

L'ancien graphe global de droits et le transport Knowledge ont été retirés du protocole actif. Le format de stockage est `flat-4`, sans migration de l'ancien format. Les concepts historiques restent documentés comme tels.

## Mécanisme exécuté

1. La source découpe localement un droit natif. Elle reste écrivain partout ; les compléments sont disponibles pour d'autres transferts.
2. Le receveur réserve un ticket. Son premier préchargement le confirme durablement ; la source persiste cette confirmation avant toute fence.
3. Chaque lot suit copie chaude → fence locale → dernier delta → certificat de données → installation. Le receveur écrit les zones installées immédiatement.
4. La clôture du morceau consomme le droit de la source et installe le droit régional chez le receveur, espaces vides compris. Un premier morceau natif peut être redélégué pendant la suite du join ou du déménagement physique.
5. Un ticket fermé ne remonte plus de données. L'ancien paquet d'installation reçoit un ACK sans effet ; un préchargement sans réservation valide ne peut pas fabriquer une confirmation.

La membership diffuse identités et adresses en arrière-plan. Les réponses d'écriture donnent un indice de destination. L'écriture n'attend ni un graphe historique ni un accord global. Les droits installés restent le contrôle de sûreté.

Les lectures exactes suivent la couverture et un répertoire courant de délégations. Une absence de données ne prouve pas un espace vide. Les résumés signalent leur incomplétude quand leurs hypothèses de placement ne correspondent plus aux droits réels ; la façade utilise alors Query. Les résumés ordinaires restent asynchrones : ce mécanisme ne les rend pas linéarisables.

## Régressions trouvées pendant cette intégration

- Des écritures vers des adresses encore vides étaient refusées pendant la consolidation. Une barrière locale couvre maintenant le morceau ; les lots déjà installés continuent à écrire sans l'attendre. Le délai d'attente de 500 ms par tentative n'est pas une borne universelle de durée du gel.
- Le passage SourceIssued → installation native pouvait renvoyer NotOwner avant que le receveur soit prêt. Un refus pendant une bascule locale permet un nouvel essai après la barrière, avec vérification normale sur disque.
- Un vnode peut garder légitimement des données hors de sa cellule XOR théorique. Les résumés de placement les ignoraient et pouvaient rester incomplets. Le statut versionné des contributeurs invalide désormais ces réponses dérivées ; un ancien Complete ne peut pas lever un statut plus récent déjà observé.
- Les petits morceaux sélectionnés en rescannant tous les enregistrements risquaient un coût quadratique. Un index spatial dérivé partage son snapshot avec celui des données. Après un vrai Close/Open, un test reconstruit 4 096 en-têtes une fois, puis 64 BeginCausal examinent 1 029 nœuds d'index au total.
- Trois défauts de fixtures sont apparus au premier race global : remplacement d'un transport pendant son utilisation asynchrone, échantillon géométrique mal réparti, partition terminée avant sa première observation. Les fixtures corrigées conservent les assertions de sûreté et leurs deadlines ; la partition est maintenant observée explicitement.

## Validation finale

- Suite Go globale : passée, 53,107 s (`go test ./...`).
- `go vet ./...` : passé.
- `go test -race ./... -timeout=10m` : passé entièrement sur `e65dfd3`, arbre propre ; mesh 184,153 s, API 30,583 s. La première campagne rejetée est conservée dans les logs.
- Fuzz : 30 s sur le scénario (3 489 cas), puis 30 s sur la compaction des versions du répertoire (1 247 800 cas), sans échec.
- PowerFS64 : deux exécutions passent, même transcript SHA-256 `11037c2e738668fd2e60730ebd707c5169e3df8304d76ce9126adcf8ca49aa3a`. Chacune : 3 522 étapes, 284 écritures dont 23 précoces, 51 transferts, 43 requêtes et 25 réponses perdues, 167 crashs/reprises, 10 coupures I/O, 153 rejeux réels et 171 collectes.
- HTTP64 strict : 1 480 offres et 1 480 ACK ; zéro échec, rejet, ACK perdu/dupliqué, double écrivain et gel restant. Les 128 seeds et les 1 480 écritures donnent 1 608 records observés. Les disques sont rouverts et relus. Les 64 joins terminent vers 65 s ; audit à 103,809 s, test complet à 108,310 s.
- TLC : 31 configurations exécutées, 11 positives et 20 contre-exemples attendus. Les modèles sont bornés, avec hypothèses de pairs honnêtes ; les modèles historiques et les noyaux abstraits ne constituent pas un raffinement automatique du code Go.

Le HTTP64 v5 a conservé 1 817 ACK sans perte ni doublon, mais a refusé 124 écritures sur 1 941 offres. Son ancien test passait seulement la sûreté. Le test final exige aussi zéro échec, zéro rejet et autant d'ACK que d'offres ; v5 n'est donc pas une validation de disponibilité.

La latence client pendant ce scénario est distincte du gel : p99 ACK 566,489 ms, maximum 752,746 ms ; p99 depuis l’arrivée prévue 572,225 ms. Elle inclut routage, files, retries et les fautes injectées. Le run comprend une partition de 750 ms, une traîne I/O supplémentaire de 25 ms durant 750 ms et des réponses d’installation perdues. La réconciliation finale du placement, utilisée pour l’audit, termine à 76,739 s ; elle ne conditionne pas les premiers ACK.

## Comparaison de coût

Base `50533fe` (ancien protocole exécuté), cible `7aaabf7` (même code de production que `e65dfd3`). Go 1.26, Apple M1 Pro, GOMAXPROCS=4. Paire avant-b/après-b exécutée séquentiellement sans autre campagne de tests. Trois échantillons par cas ; médianes ci-dessous. Le benchmark de façade ajouté a été copié à l'identique dans le checkout de référence. Les fsync et les microbenchmarks restent variables ; ces chiffres ne sont pas un SLA distribué.

| Cas | Avant | Après | Variation |
| --- | ---: | ---: | ---: |
| `BenchmarkCausalInsert/same-zone/durable-update` | 4206.05 µs | 4402.04 µs | +4.66 % |
| `BenchmarkCausalInsert/distinct-zones/durable-update` | 4183.04 µs | 4349.18 µs | +3.97 % |
| `BenchmarkInventoryPayloadProjection/metrics-only=false` | 107.28 µs | 125.68 µs | +17.15 % |
| `BenchmarkInventoryPayloadProjection/metrics-only=true` | 49.77 µs | 49.09 µs | -1.37 % |
| `BenchmarkCausalInsert/same-zone/logical-update` | 33.32 µs | 31.52 µs | -5.38 % |
| `BenchmarkCausalInsert/distinct-zones/logical-update` | 29.69 µs | 34.49 µs | +16.19 % |
| `BenchmarkSteadyRead/ordinary` | 11.22 µs | 9.05 µs | -19.33 % |
| `BenchmarkSteadyRead/fresh` | 70.00 µs | 67.58 µs | -3.45 % |

Le coût logique de création de nouvelles zones augmente avec la maintenance de l'index. Les écritures durables et les lectures doivent être évaluées séparément. Le premier lot après-a chevauchait la suite stockage et n'a pas été retenu pour cette comparaison.

Comptage brut Go hors `_test.go`, blancs et commentaires compris, tous packages : 47 571 → 44 705 lignes, soit −2 866 (−6,0 %). Diff Go total, tests compris : 10 058 ajoutées et 17 101 supprimées avant les derniers ajustements de fixtures. Le checkout temporaire de mesure a été supprimé ; les logs et le benchmark reproductible sont conservés.

## Limites à garder explicites

- Pairs honnêtes, digests sans garanties byzantines. Le reçu local accepté remplace la preuve d'une descendance complète jusqu'à la genèse.
- Les compteurs et fences sont durables. Restaurer une image ancienne sous la même identité de disque n'est pas permis ; une identité neuve est nécessaire en cas de recul d'état.
- Un receveur indisponible après cession irréversible peut rendre le morceau indisponible jusqu'à sa récupération. Un timeout ne réactive pas l'ancienne source.
- Des zones partageant exactement la même adresse XOR sont géométriquement indivisibles. Les cibles 512 zones / 1 Mio restent consultatives sous ce cas ou sous écritures concurrentes. Aucune borne universelle de gel n'est démontrée.
- L'index évite les rescans répétés dans les cas qualifiés ; sa construction froide reste proportionnelle aux en-têtes. Les masques arbitraires n'ont pas de borne sublinéaire universelle.
- La membership contient aussi les tombstones des identités retirées. Les droits fragmentés, les délégations actuelles et les opérations ouvertes ont un coût ; la taille n'est pas constante pour une histoire arbitraire de nouvelles identités.
- HTTP64 représente 64 services sur un seul hôte, avec HTTP réel, fichiers/fsync réels et 32 disques sérialisés simulés. Il ne mesure pas le débit de 64 machines AWS.

Les résultats structurés et empreintes des journaux sont dans [local-permissions-20261125.json](results/local-permissions-20261125.json). Les mécanismes sont décrits dans [permissions locales](../protocol/local-permissions.md) et [qualification détaillée](../protocol/local-permissions-verification.md).

Pour reproduire les contrôles Go/TLC : `TLA2TOOLS_JAR=/chemin/tla2tools.jar bash scripts/research/check-handoff.sh --full`. Le SHA du jar attendu est documenté dans `spec/handoff/README.md`. Le laboratoire HTTP64 est opt-in dans `indexus/lab64_test.go` ; son run qualifié utilise BASE_RATE=40, PHASE_SECONDS=2, SEED_ZONES=128, UNIQUE_ZONES=1, DIRECT=1, TAIL_RATE=5 et un nouveau dossier de données.


Commande exacte du laboratoire HTTP qualifié, depuis un dossier de données neuf :

```sh
INDEXUS_LAB64=1 \
INDEXUS_BENCH_NO_USAGE_SCAN=1 \
INDEXUS_LAB_BASE_RATE=40 \
INDEXUS_LAB_PHASE_SECONDS=2 \
INDEXUS_LAB_SEED_ZONES=128 \
INDEXUS_LAB_UNIQUE_ZONES=1 \
INDEXUS_LAB_DIRECT=1 \
INDEXUS_LAB_TAIL_RATE=5 \
INDEXUS_LAB_REQUEST_TIMEOUT_MS=10000 \
INDEXUS_LAB_TIMEOUT_SECONDS=240 \
INDEXUS_LAB_OUT="$(mktemp -d)" \
go test ./indexus -run '^Test64NodeDurableHTTPRecovery$' -count=1 -v -timeout=5m
```
