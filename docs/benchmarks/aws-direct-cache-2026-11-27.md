# Routage direct, montée en charge et placement du trafic

Le 27 novembre, le run AWS `aws-cache-direct-1127-0549` de la révision
`84af996` a franchi le palier 4 000/s avec une file client vide. Son arrêt
sur alerte pendant la rampe suivante laisse 8 000 et 16 000/s à qualifier.

## Comparaison AWS

Même configuration que le [passage précédent](aws-elastic-direct-2026-11-27.md).
Seize vnodes initiaux sur quatre hôtes, cinq slots par hôte, routage direct,
H3 entre pairs, budget de 8 USD, TTL de 30 minutes. La charge est uniforme.

| Fenêtre | p99 client précédent | p99 client corrigé | p99 HTTP corrigé | p99 apprentissage corrigé |
| --- | ---: | ---: | ---: | ---: |
| 0–120 s | 8,192 ms | 8,192 ms | 8,192 ms | 0,008 ms |
| 210–240 s | 1 048,576 ms | 32,768 ms | 32,768 ms | 0,016 ms |
| 240–297 s | 2 097,152 ms | 32,768 ms | 32,768 ms | 0,032 ms |

Bornes supérieures des histogrammes agrégés sur chaque fenêtre. Les calendriers
réels de joins varient entre essais. Le cache client apprend les points dans
sa table exacte et conserve la couverture régionale. Le protocole reste celui
du passage précédent.

À 322 s, le générateur publie 5 433 arrivées et 5 431 ACK dans la dernière
seconde. Après vidage, 1 008 214 offertes, autant acquittées, aucune refusée
par la file ni épuisée après retries. Le maximum de file publié est zéro.
Le dashboard a observé jusqu’à 38 vnodes et 29 hôtes. La sentinelle a arrêté
le run avant l’audit final de relecture.

## Alerte et ressources

La cible 10.77.3.253:19100 a enregistré « join not needed any more, leaving »
et est sortie avec le code 0. Les échantillons indiquent deux écritures
relayées, zéro appliquée, aucun droit reçu observé et client_ready faux.
La sentinelle a déclaré node_down après trois sondes. Son relais externe
rafraîchissait la liste des slots environ toutes les cinq secondes. Le dernier
échantillon du relais avant la sortie et le suivant encadrent les trois sondes
en échec. Pour le passage suivant, ce relais lit les générations et les codes
de sortie directement auprès des agents à chaque sondage et conserve les slots
connus en cas d’échec réseau. La correction du relais a été testée localement.
Le collecteur de profils n’a pas capturé ce passage car le premier fichier de
charge était vide à sa première lecture. Son démarrage tolère désormais ce cas.

Toutes les instances du run sont terminées. La suppression du VPC, du groupe
de sécurité, du modèle de lancement, du rôle, du profil IAM et du bucket a été
vérifiée par les API AWS.

## Qualification locale et trafic futur

Le test de délégation hors placement nécessitait qu’un donneur précis détienne
la région avant de l’envoyer explicitement. En retardant les échanges de
membership, on reproduit le cas où le troisième vnode a déjà obtenu ce droit.
Le montage synchronise désormais les deux participants avant le second join.
Les assertions de lecture et d’autorité sont inchangées. Quarante répétitions,
trois passages sous race et la suite complète réussissent.

Le banc multiprocessus accepte désormais `placement.py --linear`. Avec cinq
vnodes initiaux, deux hôtes logiques partageant une VM Linux et la moitié des
arrivées sous le préfixe Hot, la demande monte de 200 à environ 640/s en 45 s.
18 904 ACK ont été relus, les 17 totaux exacts sont conformes, aucun double
propriétaire observé. p50/p95/p99 du client : 8,192/65,536/131,072 ms.

Entre 8–15 s puis 25–32 s, le premier donneur passe de 191 à 58 écritures
appliquées/s et le premier receveur atteint 235/s. Entre 37–44 s le deuxième
receveur atteint 282/s sur la même machine de réserve. Un split peut donc
déplacer le préfixe chaud presque entier.

Une projection sur la trace acquittée conserve la géométrie initiale et
compare les cibles canoniques d’un seul split. Sur les 3 000 premières
arrivées, 1 873 relevaient du donneur selon XOR. La cible demandée (bit 2)
aurait récupéré 90,43 % des 9 817 arrivées ultérieures de ce donneur.
Le premier bit admissible dont la part passée est à moins de 1/√1873 de 50 %
(bit 18, après les trois caractères du préfixe) en récupère 50,43 %.
Sur trois fenêtres glissantes de 512 écritures passées, le même choix donne
48,47–49,35 % sur les fenêtres suivantes. Elles se chevauchent.

Ce rejeu compare des cibles de vnode à topologie fixe. Il reste à mesurer
le coût de copie et les divisions suivantes, ainsi que la stabilité si le
préfixe chaud change. Le gain est à tester avec la charge déjà présente sur
les deux machines avant de changer le planificateur.
