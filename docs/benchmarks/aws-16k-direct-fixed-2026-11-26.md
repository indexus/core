# Essai direct interrompu pendant la montée vers 8k/s

Campagne `exp-16k-direct-fixed-20261126-134721`, eu-west-3, source figée
`5e5890d`, loader `2362b35`. Le scénario prévu était 2k → 4k → 8k → 16k/s,
avec 120 s par plateau, rampes de 60 s et traîne de 180 s. Il est interrompu
avant le plateau 16k/s : **ce n'est pas une qualification à 16k/s**.

Les nœuds servent H3, les entrées de charge sont en mode `direct`, compaction
active. Deux i4i.xlarge initiaux, deux slots chacun, jusqu'à 32 machines et
64 vnodes, un c6i.4xlarge de mesure, TTL 30 minutes. L'enveloppe autorisée est
8 USD ; le planificateur utilise 7,25 USD pour le calcul de capacité. Le maximum
EC2 nominal à flotte complète pendant 30 minutes est 6,772 USD, hors annexes.
Les prix et le quota régional sont vérifiés avant création. Seules les ressources
de cette campagne sont concernées par son nettoyage.

L'infrastructure ajoute un accès SSM temporaire au rôle de campagne, conserve
les ledgers d'ACK et expose l'environnement du scénario localement au collecteur.
Ces modifications sont archivées dans `infra.patch` ; les binaires de protocole
restent ceux de la source figée. Le profilage ciblé et les captures de piles
peuvent perturber les fenêtres où ils tournent. Le dashboard et les traces
individuelles sont conservés avec les artefacts.

## Ce qui a été observé

Le palier 2k/s et le palier 4k/s suivent la demande. Pendant les joins, des
échantillons de p99 montent jusqu'aux buckets 262 et 524 ms respectivement.
Sur les points de plateau 4k/s entre t=180 et 300 s, le ratio instantané
ACK/demande reste au moins 99,4 %. Ce ratio n'est pas un taux final de réussite :
des requêtes peuvent traverser deux fenêtres de mesure.

Pendant la rampe suivante, la file du loader atteint 64 000 éléments avec
16 000 opérations en vol. À t≈353 s : demande 7 367/s, ACK 3 178/s, shed 4 189/s,
p99 dans le bucket 8 388,608 ms et p99 intentionnel 16 777,216 ms. Les points
suivants atteignent un p99 intentionnel de 33 554,432 ms. Les métriques sont
fraîches ; ce ne sont pas des extrapolations de la courbe.

Le cluster compte alors 37 vnodes. Les relais restent sous 1 % et le loader
utilise environ un quart de ses 16 cœurs. Cela ne correspond pas à l'ancienne
tempête de relais. La capture à 13:55:15 UTC montre une machine à 99,8 % de
CPU et une autre à 92,3 %, alors que beaucoup d'autres sont peu utilisées.
Dans cette source, les facts additionnent le CPU des processus vnodes ;
ils ne mesurent pas encore le CPU total de l'hôte.
Le déséquilibre des items atteint environ 4,8 fois la moyenne. La capacité
autorisée n'est pas encore entièrement utilisée en vnodes actifs.

Les profils des deux vnodes sur la machine la plus chargée attribuent
respectivement 37,9 % et 35,9 % de CPU échantillonné à la chaîne autoscale →
`PlanRegionalAdd` → `hottestReliefLeaf`. La lecture du code confirme que chaque
zone candidate reparcourt toutes les autres via `isLeafAmong` : coût quadratique
en zones possédées. C'est un poste mesuré important, pas la preuve qu'il explique
tout le décrochage. Les profils contiennent aussi du décodage réseau et du GC.

Les identifiants des opérations ouvertes changent entre les captures. Les deux
opérations des vnodes les plus chauds ont moins d'une seconde dans la capture
initiale, mais une autre copie chaude atteint 30,2 s. Après l'arrêt de charge,
la capture finale relève encore une installation régionale de 9 s et une copie
chaude de 11,8 s. Ces durées d'opération ne sont pas des mesures de gel de chaque
zone. Il n'est pas établi que les mêmes opérations restent bloquées des minutes.

## Arrêt et limites de l'audit

Le générateur reçoit SIGINT à 13:55:17 UTC, avant la montée à 16k/s. Son résumé
annonce 1 446 030 demandes, 1 235 768 ACK, 145 968 shed, 16 313 tentatives
épuisées et 47 981 éléments restés en file. Le ledger sauvegardé contient
1 236 899 lignes : il ne faut pas l'assimiler au compteur du résumé sans
réconciliation, notamment au moment de l'interruption et du drainage.

Les nœuds continuent à demander du renfort : le seuil de 20 000 items reste
dépassé même sans nouvelle charge. Le provisionneur est arrêté à 14:00:21 UTC
pour limiter les nouvelles créations et laisser les transferts finir.
Le délai de repos de 300 s est épuisé avec encore des transferts ouverts.
Malgré son texte `quiet after 300s`, le fichier `quiet.log` n'établit donc pas
que le cluster était effectivement au repos.

L'audit démarre ensuite ; son log signale 184 totaux différents avant une
vérification complète des clés. Il est interrompu pour arrêter la campagne.
Des transferts sont encore actifs et des écritures en vol peuvent avoir une
issue inconnue du client. **Aucun audit final valide n'est revendiqué** : ces
données ne prouvent ni la conservation de tous les ACK ni leur perte.
Les ledgers, traces, états, profils et piles restent disponibles pour la suite.

## Corrections intégrées ensuite, non mesurées par cette campagne

Claude fournit `0980a46`, qui marque les préfixes en une passe pour sélectionner
une feuille sans comparer toutes les paires. Les tests comparent le résultat
à la définition précédente, racines, doublons et collections multiples compris.
Ce correctif rejoint les changements de structure et de stockage de Claude
dans la branche de fusion. Les résultats AWS ci-dessus restent ceux de
`5e5890d` ; aucun gain AWS de ces nouveaux changements n'est encore établi.

Le prochain scénario utilise un seuil de volume plus élevé et un seuil de débit
distinct : voir [le dimensionnement](aws-16k-sizing-2026-11-26.md). Il faut
requalifier les lectures, les ACK et la clôture avec cette source avant de
conclure que le problème est entièrement résolu.

Les 33 instances sont toutes `terminated`. Le réseau, le modèle de lancement,
le bucket, le profil et le rôle IAM sont vérifiés supprimés indépendamment.
L’estimation EC2 haute jusqu’à cette observation est **3,95 USD**, hors stockage,
S3 et transfert ; ce n’est pas une facture.

[Résultats compacts et vérification du nettoyage](aws-16k-direct-fixed-2026-11-26.json).
