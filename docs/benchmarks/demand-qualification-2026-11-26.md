# Demande réelle : capacité utile mesurée, audit final à requalifier

> Suite : le [passage unique avec l’audit corrigé](settled-demand-2026-11-26.md)
> réussit sur `0b4ddb4`. Les échecs et observations ci-dessous restent ceux du
> premier passage et sont conservés pour la comparaison.

## Résultat

L’essai local reste **échoué**, sans relance automatique ni AWS : l’audit a
rencontré HTTP 503 sur le total exact de la collection chaude. Les 17999 ACK
sont tous relus individuellement, sans erreur ; aucune double propriété n’est
observée. Cela ne transforme pas l’audit complet en réussite.

En revanche, la chaîne demande → placement → transfert → trafic est réellement
exercée : cinq vnodes initiaux, puis un receveur sur la réserve et deux renforts
locaux supplémentaires sur cette même réserve. Les deux premiers nouveaux
vnodes appliquent des écritures avant la fin de leur join.

Source des quatre binaires : `e6e86d7`, incluant les quatre timers indépendants.
Trois conteneurs, deux agents à cinq slots chacun, sur **une seule VM Docker**.
Ni CPU ni disque physiquement indépendants, ni réseau H3/WAN. Sous les écritures, le CPU reste inférieur à 34 % dans les sondages.
L’audit provoque ensuite des pointes distinctes, précisées plus bas.

## Paramètres fixés avant le départ

- 400 écritures/s pendant 45 s, mode direct, graine 19.
- Scénario spike avec facteur 1 : moitié des écritures sous le préfixe `Hot`,
  sur des zones feuilles distinctes ; autre moitié répartie sur 16 collections.
  Il y a donc 17 collections, pas une distribution uniforme.
- Signal demandé : 150 écritures/s/vnode ; pas de plafond en items.
- CPU cible 60 %, admission mémoire/disque, deux jetons de copie et autres
  seuils de production inchangés. GOMAXPROCS=2 et GOMEMLIMIT=512MiB/processus.
- Réserve de slots vide et mesurée admissible avant la charge ; deux pools
  maximum, pas de création de machine ni d’appel AWS.

Le signal de demande sert à qualifier le chemin local sans fabriquer un CPU
indépendant pour chaque conteneur. La valeur 150 est un paramètre de banc, pas
une recommandation de capacité de production.

## Déroulement mesuré

Temps relatifs au départ de l’observation de charge ; l’échantillonnage des
métriques est d’environ 0,5 s. L’âge à la première application vient du compteur
monotone du processus, plus précis que ces sondages.

| Vnode de réserve | Première observation | Première écriture appliquée, âge du processus | Fin du join observée | Écritures appliquées cumulées |
| --- | ---: | ---: | ---: | ---: |
| Slot 0 | 13,71 s | 258,5 ms | 15,91 s | 3795 |
| Slot 1 | 28,98 s | 291,8 ms | 33,29 s | 2967 |
| Slot 2 | 45,33 s | aucune après sa naissance | 56,46 s | 0 |

Au premier sondage, les slots 0 et 1 sont encore `joining=true` et ont déjà
appliqué des écritures. C’est une preuve d’activité avant la clôture complète,
pas une date exacte du premier droit installé ni du premier ACK reçu au client.

Le troisième renfort arrive en toute fin de charge. Il installe des droits et
des données, mais n’applique aucune nouvelle écriture dans cet essai : le
loader s’arrête à 45 s. Le présenter comme un soulagement mesuré serait faux.

Le provisioner observe et applique un placement `new` sur la réserve, puis
deux placements `local` sur cette réserve. Les demandes viennent cette fois des
contrôleurs des vnodes ; `Wanted()` et la validation du handoff restent actifs.
Ce scénario de demande `writes` ne mesure pas la coalescence `host_pressure`.

## Écritures et ressources

17999 arrivées, 17999 ACK, zéro shed, zéro épuisement définitif et aucune requête
admise restant dans la file du générateur à la fin. Les histogrammes périodiques
ne couvrent pas la dernière fraction non publiée ; leurs quantiles sont les
bornes supérieures des buckets logarithmiques.

| Fenêtre de charge | p50 | p95 | p99 |
| --- | ---: | ---: | ---: |
| Avant le premier renfort, 0–12 s | 8,192 ms | 8,192 ms | 16,384 ms |
| Premier renfort, 12–28 s | 8,192 ms | 16,384 ms | 32,768 ms |
| Renfort suivant, 28–45 s | 8,192 ms | 16,384 ms | 32,768 ms |

Un passage à 400/s n’établit ni une capacité AWS à 16k/s ni une loi de p99 en
fonction de la taille du réseau. Les CPU `/facts` sont ceux de la VM partagée,
pas deux mesures de capacité physique à additionner. Sous charge, le pic CPU VM observé est de 34,0 %. Après l’arrêt des
écritures, les lecteurs concurrents de l’audit et le dernier transfert portent
certains sondages à **68,5 %** : la cible de 60 % n’est pas respectée pendant
cette phase. Aucun seuil n’est relevé pour le masquer. Le coût des vérifications
co-résidentes doit être distingué de celui des écritures ; une cible d’admission
n’est pas un plafond dur instantané. Les compteurs du budget de copies montrent
zéro attente, timeout et bypass occupé, avec 437 partages co-résidents. Ce petit
essai ne prouve donc pas le comportement du budget sous contention durable.
Les valeurs détaillées et les snapshots figurent dans le JSON.
Ces temps d’ACK ne sont pas des bornes de gel de toutes les zones.

## Trafic de contrôle et coût de l’audit

Sur les compteurs cumulés émis par les huit vnodes en fin d’observation,
requêtes + réponses : membership **458178 octets**, preload **11035533 octets**,
install-zone **18913450 octets**. La membership représente environ 1,5 % de
ces deux chemins de copie/install dans cet essai, sans prétendre au même ratio
à toute taille de réseau. Ce sont des compteurs RPC, pas des octets TLS/IP.

`inventory-prefix` atteint **136178159 octets** : les lectures de vérification
et l’audit supplémentaire de la collection indisponible ne sont pas gratuits.
La relecture point par point génère de nombreux appels ; ce trafic ne doit pas
être présenté comme de la Knowledge de transfert. Les 71 tentatives relayées
observées sont des compteurs de service, pas un pourcentage mesuré de trajets
clients directs. Aucun trafic de contrôle n’est supprimé pour améliorer le score.

## Pourquoi l’audit a échoué

### Faits observés

L’audit relit les 17999 clés acquittées sans manque ni erreur. Ensuite,
`collectionTotal` reçoit **HTTP 503**, pas un total numérique de zéro, pour
`Placeh0000000000`, dont 8998 écritures sont attendues. Le compteur historique
`count_mismatches: [8998, 0]` mélangeait une valeur inconnue et un vrai zéro.

Le troisième transfert commence à la fin de la charge et travaille encore
entre environ 45 et 55 s : les sondages montrent des fences, des mounts et un
handoff ouvert. L’audit des totaux est lancé après les lectures de clés,
vers 50 s, pendant ce transfert.

Le code de l’audit attendait les transferts **seulement s’il trouvait des zones
en double**. Aucune zone n’était vue en double, donc il ne vérifiait pas le
calme. `moving_at_ownership=0` était alors sa valeur initiale, non une observation.
Les dernières métriques vers 59 s montrent bien zéro transfert ouvert/gelé,
mais elles arrivent après le contrôle qui a échoué.

### Ce que cela permet de conclure

Le banc ne respectait pas sa phase d’attente avant l’audit final. Une lecture
exacte peut refuser explicitement pendant un transfert ; elle ne doit jamais
transformer cette indisponibilité en un total partiel réussi.

Le corps exact du 503 n’a pas été conservé par l’ancien lecteur. Les transferts
en cours sont démontrés ; leur rôle précis dans cette réponse reste à confirmer
avec le message serveur. On ne conclut pas à une perte de données ni à un
bug permanent de couverture à partir du statut HTTP seul.

## Relecture hors ligne sur copies des stores

Les huit stores capturés sont copiés dans des répertoires temporaires, puis
ouverts sans jobs de liveness, résumés ou autoscale. Un transport JSON relie les
services en mémoire ; aucun conteneur ni réseau physique n’est redémarré.
Les fichiers originaux ne sont pas modifiés et leurs clés privées ne sont pas
publiées.

Les huit entrées retrouvent chacune exactement **8998 records chauds**, sans
erreur. Ce résultat valide la lisibilité de cet état durable au calme dans ce
replay ; il ne rejoue pas la concurrence ni les délais réseau de l’essai.
[Test opt-in et journal](demand-qualification-20261126/offline-replay.log).

## Correctifs du harnais

Aucun changement du protocole, des droits ou des seuils :

1. L’audit attend désormais, même sans doublon, que les moniteurs observés ne
   signalent plus de handoff ouvert, de gel ou de join en cours, avec le budget
   `-settle` existant. Un timeout échoue ; il ne clôt aucun transfert.
2. Une télémétrie inconnue, invalide ou inaccessible ne certifie plus le calme.
   Les observations sont bornées en concurrence et en temps, hors des chemins
   d’écriture. Le nombre de sondages et leur durée sont publiés.
3. Une indisponibilité de total apparaît dans `total_errors`, séparément des
   différences numériques. L’audit échoue toujours dans les deux cas ; aucun
   retry de total n’est ajouté pour rendre le résultat vert.
4. Le lecteur conserve le message JSON d’erreur du serveur, en plus du statut.

Il s’agit de correctifs fonctionnels de mesure et d’ordonnancement de l’audit,
pas d’un gain mesuré du writer. Le calme observé n’est pas un snapshot atomique
mondial : si une nouvelle mutation rend le total indisponible ensuite, l’audit
échoue encore et conserve la cause. Le watermark des pages reste conservateur ;
son éventuelle invalidation par du travail annexe n’est pas corrigée ici.

Les tests ajoutés couvrent une attente sans doublon, gel seul, join seul,
moniteur inconnu/absent, deadline, conservation du 503 et distinction entre un
zéro réel et une valeur inconnue. Les assertions existantes ne sont pas affaiblies.
Suite Go complète, build et tests `-race` du paquet experiment verts.
**L’essai physique n’a pas été relancé après l’échec** : le nouvel audit reste
à qualifier sur ce scénario. Aucun essai AWS n’a été lancé.

## Replay et suite

[Replay interactif](demand-qualification-20261126/live.html), généré avec les
mêmes composants que le dashboard AWS/simulation ; cinq puis huit vnodes,
regroupés par pools, avec les transferts effectivement observés. Le statut final
reste rouge. Schéma, groupes et syntaxe JavaScript vérifiés ; le navigateur
intégré bloque `file://`, donc le rendu n’a pas été inspecté visuellement.

[Résumé structuré](demand-qualification-20261126/summary.json),
[empreintes et traces](demand-qualification-20261126/manifest.json).
Les stores complets restent dans les outputs locaux. Les conteneurs et le réseau
de cet essai ont été supprimés. Le prochain pas est un seul passage avec l’audit
corrigé, sans relever les seuils ; il devra confirmer les totaux finaux et dater
séparément lancement, première application et fin du join.
