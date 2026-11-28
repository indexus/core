# Banc AWS mixte à 8 000 écritures/s

Une passe le 28 novembre 2026 à Paris (`eu-west-3`), run
`aws-8k-point-1128-174915`, commit Core et générateur `70680f80`.
Le réseau H3/QUIC utilisait des volumes EBS. Il a démarré avec 16 vnodes
sur quatre `i4i.xlarge` et a atteint 102 vnodes sur dix machines de données.
Un générateur `c7i.2xlarge` émettait des écritures en boucle ouverte et visait
500 lectures ponctuelles/s sur des clés récemment acquittées.

Trois paliers de 45 s, séparés par des rampes de 30 s. Le client choisissait le
dernier détenteur connu et suivait les indications de routage. Les lectures de
charge utilisaient `/records/direct`. Les lectures d’audit suivaient leur propre
chemin de vérification.

| Écritures offertes/s | ACK/s | p50 / p95 / p99 écriture (ms) | p99 depuis l’arrivée prévue (ms) | Lectures réussies/s | p50 / p90 / p95 / p99 lecture (ms) |
| ---: | ---: | --- | ---: | ---: | --- |
| 2 000 | 1 999,4 | 8,192 / 16,384 / 16,384 | 32,768 | 499,1 | 1,024 / 2,048 / 2,048 / 4,096 |
| 4 000 | 4 000,0 | 8,192 / 16,384 / 16,384 | 32,768 | 499,2 | 1,024 / 2,048 / 2,048 / 4,096 |
| 8 000 | 7 999,9 | 8,192 / 16,384 / 16,384 | 32,768 | 500,2 | 1,024 / 2,048 / 2,048 / 8,192 |

Les percentiles sont les bornes hautes des classes des histogrammes agrégés
par palier. Les latences de lecture portent sur les succès. Le débit d’ACK
est compté à leur réception, donc des réponses peuvent franchir une limite de
palier. Le bilan après vidage de la file est de 908 164 écritures offertes et
acquittées, zéro écartée et zéro tentative finale épuisée.

Le plus long intervalle sans écrivain par lot relevé dans les journaux
(`hold_max`) est de 116 ms. Le dernier relevé indique zéro transfert ouvert
et zéro gel. L’audit a retrouvé les 512 clés échantillonnées, rapproché les
totaux de 257 collections avec 908 214 écritures acquittées, sondes de fraîcheur
comprises, et détecté zéro zone à double propriétaire.

Le calcul EC2 est estimé entre 0,4174 et 0,4193 USD, générateur compris.
L’estimation utilise les prix et durées des instances, hors stockage et
réseau. La suppression des instances, des volumes EBS, du bucket et des
ressources réseau de la campagne a été vérifiée.

Cette passe qualifie la charge mixte jusqu’à 8 000 écritures/s dans cette
configuration. Le gain de latence causé par la lecture directe demanderait
un essai apparié à configuration identique. Le [comparatif de recherche
vectorielle](search-campaign.md) utilise un autre protocole expérimental.

[Mesures, configuration et contrôle de nettoyage](aws-direct-reads-8k-2026-11-28.json) · [Politiques de lecture](../protocol/read-modes.md)
