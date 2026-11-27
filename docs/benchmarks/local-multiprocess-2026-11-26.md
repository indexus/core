# Qualification locale : cinq processus vnode et calibration des files

## Ce qui a avancé

Neuf essais terminés, **34 997 ACK relus exactement**, aucun ACK manquant,
aucune double propriété à l’audit, aucun transfert en mouvement à l’audit final.
Les lectures ponctuelles périodiques n’ont signalé ni absence ni erreur.
Ce sont des observations locales, pas une preuve supplémentaire de sûreté.
Aucun essai AWS lancé ; aucune règle d’autorité ni aucun seuil modifié.

Le banc a aussi révélé un défaut du **loader** : une redirection de lecture
superficielle était comptée comme un total nul. Le correctif est dans
`cmd/experiment`, sans changement du protocole.

## Environnement et portée

Source des binaires node/issuer initiaux : `4f6a615`. Linux arm64 dans Docker
Desktop, huit CPU visibles, VM d’environ 8 Go, `cpu.max = max 100000`.
Un seul conteneur jetable héberge l’issuer, l’agent, cinq processus vnode,
un loader et un enregistreur des demandes de scale. Les deux services Docker
préexistants restent actifs et ne sont pas arrêtés : le CPU machine les inclut.
Le stockage est celui de la VM Docker, pas du NVMe i4i. Réseau HTTP loopback,
pas de H3 ni de lien distant. Aucun swap utilisé dans l’observation de pression
(SwapTotal = SwapFree). Les empreintes des binaires sont dans le manifest.

Charge : 100 écritures/s, huit collections, graine 19, cinq vnodes, quatre joins
simultanés à six secondes ; 35 s par variante normale. Un lecteur à dix
requêtes/s mélange lectures ponctuelles et 20 % de totaux superficiels.
Les vnodes ont des identités nouvelles à chaque essai : le placement n’est
pas figé. Un passage par variante sert à qualifier les chemins, pas à choisir
le meilleur réglage CPU. Les histogrammes périodiques excluent leur dernière
fraction non publiée ; les nombres de mesures sont dans le JSON.

Le récepteur de demandes de scale est **diagnostique** : il journalise et répond
503, sans créer de capacité. La fusion des demandes par machine, les réserves,
le boot, le placement et les déplacements entre machines restent à qualifier.

## Matrice mesurée

Bornes supérieures des buckets de latence ACK, en millisecondes :

| Jetons | GOMAXPROCS demandé | ACK | p50 / p95 / p99 | Pic CPU VM observé | Attentes / timeouts / partages |
| ---: | --- | ---: | --- | ---: | --- |
| 1 | 1 | 3500 | 8.192 / 16.384 / 32.768 | 19.1 % | 0 / 0 / 52 |
| 1 | 2 | 3500 | 8.192 / 16.384 / 16.384 | 22.6 % | 1 / 0 / 46 |
| 1 | default | 3499 | 8.192 / 16.384 / 32.768 | 24.0 % | 1 / 0 / 48 |
| 2 | 1 | 3499 | 8.192 / 16.384 / 32.768 | 21.3 % | 0 / 0 / 40 |
| 2 | 2 | 3500 | 8.192 / 16.384 / 32.768 | 16.0 % | 0 / 0 / 51 |
| 2 | default | 3500 | 8.192 / 16.384 / 32.768 | 11.0 % | 0 / 0 / 40 |

L’écart 3499/3500 vient du nombre d’arrivées publié par le générateur à l’arrêt :
chaque essai acquitte exactement toutes ses arrivées, avec zéro shed et zéro
épuisement définitif. Aucune comparaison avec les anciens p99 AWS n’est déduite
à partir de cette petite charge et de ce disque.

Les partages sont de vrais transferts source/receveur co-résidents. Les tests
`TestCopyBudgetSeparateProcessesAndCrash`, exécutés aussi dans Linux et sous
`-race`, utilisent des enfants OS distincts : exclusion pour un/deux jetons,
partage du même transfert, puis réacquisition après SIGKILL d’un détenteur.

**Limite du budget, lue dans le code :** c’est une admission souple. Après dix
secondes, une source continue sans jeton ; un receveur ou une reprise ne patiente
pas. Zéro timeout dans ces petits essais ne prouve donc pas un plafond strict
de copies. Les sondages à une seconde ratent les phases courtes : le maximum
exact de copies simultanées n’a pas été mesuré. Les flocks sont qualifiés,
pas le coût du bypass sous une longue saturation.

## Pression machine mesurée, sans changer les seuils

Un essai séparé de 70 s ajoute six goroutines CPU pendant 40 s : pic VM **82,8 %**.
Neuf demandes `host_pressure`, émises par deux vnodes, portent un CPU processus
inférieur à 1 % dans ces demandes. La première demande est observée environ
**33,9 s** après la première observation de l’injection. Les neuf sont des
**tentatives**, puisque l’enregistreur répond 503 ; ce n’est pas un comptage de
machines lancées ni une mesure de la coalescence du vrai provisioner.
Les 7000 ACK de cet essai sont tous retrouvés. Ce scénario volontairement
perturbé ne sert pas à choisir un p99 normal.

**Lu dans le code et visible dans les traces :** `holdLocked` efface le timer
de niveau dès qu’un signal de montée réapparaît ; `hot_for` retombe ainsi de
plusieurs secondes à zéro alors que le CPU reste vers 80 %. Certains vnodes
attendent plus longtemps que d’autres. Ce n’est pas un échec de détection de
`/proc/stat` : l’hôte est bien mesuré et des demandes partent.

**Piste, non implémentée :** garder indépendantes les durées de montée et de
niveau, pour que les petites variations ne fassent pas oublier un niveau élevé.
Avant correction : fixture déterministe avec niveau au-dessus de 60 % et
montées intermittentes, vérification de l’absence de famine et du refroidissement.
Aucun timer n’a été ajusté durant les essais.

## Une fausse perte de total dans le harnais

### Diagnostic confirmé

`indexus.meshGet` répond `contact + set:null` pour une lecture superficielle
adressée à un autre détenteur. Le client doit suivre ce contact. L’ancien
`collectionTotal` du loader ignorait le contact : le map nil était additionné
comme zéro. La lecture exacte (`deep=true, refresh=true`) prenait un autre
chemin ; les audits exacts continuaient donc à passer.

Un suivi de 35 s après la charge montre, pour une collection de 413 items,
413 sur son détenteur et de fausses valeurs zéro sur les quatre autres entrées.
Ces quatre réponses étaient des redirections, **pas des résumés en retard**.
Le script de suivi initial a d’abord échoué sur ce `null` Python ; ses données
restent dans les outputs locaux. La version finale distingue une redirection
(`null` dans le tableau d’observations) d’un total vide (`0`).

### Correction fonctionnelle du loader

`collectionTotal` suit désormais les contacts avec une échéance commune de 30 s,
refuse les cycles et les routes absentes, conserve l’authentification, les flags
shallow/exact et le schéma HTTPS. Un résultat vide explicite reste zéro.
Les nouveaux tests échouent sur l’ancienne fonction et passent après correction,
y compris les redirections TLS. Aucune assertion existante affaiblie.

Sur deux passages comparables mais non jumelés (identités différentes), les
62 requêtes de total donnent :

| Loader | Totaux au moins égaux aux ACK antérieurs | Somme des déficits |
| --- | ---: | ---: |
| Ancien (`copy2-gmp2`) | 17 / 62 | 11118 |
| Corrigé (`routed-totals`) | 51 / 62 | 40 |

La seconde valeur est une somme sur des requêtes, **pas 40 clés perdues**.
Les 3499 ACK du passage corrigé sont tous relus, les totaux exacts concordent.
Au suivi après arrêt, les huit réponses directes de détenteurs concordent à
partir du sondage à cinq secondes ; les autres entrées continuent correctement
à donner des contacts. Aucun scan exact ne les remplace dans cette observation.

**Conséquence pour l’ancien graphe de fraîcheur :** la sonde choisit une entrée
au hasard à chaque essai et attend 10 ms entre essais. Sans suivi du contact,
avec un détenteur parmi N entrées équiprobables, il faut en moyenne N essais
pour tomber dessus, même avec un résumé à jour. Le coût peut donc croître avec
N dans la mesure elle-même. C’est une inférence sous cette hypothèse de tirage,
pas une explication démontrée de toutes les anciennes latences. Le débit et la
latence ACK d’écriture ne sont pas corrigés par ce changement de lecteur.

## Comptage d’items : un autre retard, réel et consultatif

Les compteurs de monitoring restent parfois longtemps à 0 après un déplacement,
alors que `demand.applied` augmente et que les lectures exactes retrouvent les
records. `countDue` attend un calme d’une seconde ou une ancienneté de 30 s ;
le job est périodique. Il ne faut donc pas annoncer une fraîcheur garantie à
30 s exactement. À la fin du suivi au calme, les compteurs somment correctement
à 3500 (premier suivi) et 3499 (loader corrigé).

**Piste d’observabilité :** publier l’âge du dernier comptage dans le dashboard,
puis envisager un comptage incrémental séparé. Ne pas lancer un scan complet
à chaque `/metrics` pour masquer ce retard. La pression de cet essai est évaluée
sur les ressources réelles, sans seuil en nombre d’items.

## Calibration des files : apport réel et limites

Le modèle accepte maintenant une **tenue murale du writer**, sans attribuer
artificiellement toute cette durée au CPU ou au disque. Le banc fournit les
attentes, tenues, tentatives et commits par nature (`write`, `fence`, `mount`,
`collect`, `control`). Sur la matrice normale, les tenues moyennes sont de
l’ordre de 2,1–2,3 ms pour write, 5,2–6,6 ms pour mount et 2,1–3,6 ms pour collect.
Ce sont des moyennes sous petite charge, pas des plafonds ni des fsync isolés.

Le fichier de calibration relie aussi les compteurs de travail aux fixtures :

- réservation sans reçu installé : `reads = 8 + receipts × (sessions + 1)`
  sur les neuf points mesurés ; le produit n’a pas disparu de ce cas ;
- 64 reçus installés : 71 lectures avec 0, 8 et 32 sessions en attente ;
- huit zones ajoutées à un catalogue de 40000 : **30 sous-arbres ouverts**.

Ces points ne sont pas des coefficients universels en microsecondes. La fusion
immuable de frontières parcourt aussi la liste retenue quand elle s’agrandit.
Le replay emploie des moyennes et des parts égales déclarées ; il ne reconstitue
ni les salves ni les décisions de transfert. `calibrated:false` reste explicite.
Manquent notamment la répartition CPU/I/O, la distribution des fsync, la taille
des payloads, les traces de charge installée et la politique réelle du provisioner.

## Vérifications et suite proposée à Claude

`gofmt -l .` est vide ; `go build ./...` et `go test -count=1 ./...` passent
(tests lourds opt-in non activés). `-race` passe sur `cmd/experiment` complet
et sur les nouveaux tests mesh entre processus. Onze tests du modèle à files
et deux tests Python passent. Le `-race` mesh complet n’a pas été répété ici.
[Validation et portée](local-multiprocess-20261126/validation.json).

Le [mode d’emploi](../../ops/experiment/multiprocess/README.md) et
[les données](local-multiprocess-20261126/summary.json) sont versionnés.
Les traces compactées conservent les métriques par vnode, `/facts`, `/proc/stat`,
les demandes de scale et les états de slots ; les clés privées et les stores
restent seulement dans les outputs locaux. Aucun conteneur du banc ne reste actif.

Changements classés : correctif fonctionnel **du harnais** pour les referrals ;
tests multi-processus et instrumentation de mesure ; extension **du simulateur**
pour le coût mural. Aucun changement d’ordonnancement du protocole, de stockage
durable, de droit d’écrire ou de seuil.

Avant un prochain AWS : qualifier le vrai provisioner avec donneur chaud et
machine de réserve distincte, dont la fusion des demandes et le premier fragment
utile ; mesurer le bypass de copies sous contention durable ; examiner les timers
de pression sans les raccourcir à l’aveugle. Les simulations n’anticiperont pas
ces cas en distribuant automatiquement la charge dès qu’une machine est prête.
