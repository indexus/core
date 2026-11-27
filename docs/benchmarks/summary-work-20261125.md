# Réduire le travail répété et les attentes pendant les joins

Suite de [l'analyse des 964 ms](latency-vs-nodes-20261125.md), départ `4c28fc2`.
Ce chantier conserve les remontées hiérarchiques et les audits de lecture :
on réduit leur travail préparatoire, pas les garanties qu'elles fournissent.
Aucun essai AWS n'est lancé ici.

## Pourquoi 74 Gio d'allocations

Le profil du run dynamique à 16 nœuds et 2 Mio/store estime **74,3 Gio alloués
pendant toute la vie du test**, préparation et audit inclus, soit environ
246 millions d'objets. Il ne mesure ni 74 Gio de RAM simultanément occupée,
ni 74 Gio transférés. Les objets sont créés, utilisés et récupérés.

Les mêmes données étaient redécouvertes par des boucles finies imbriquées :
polls des parents × collections × préfixes × filtrage des membres pour chaque
préfixe. Les évictions du cache disque de 2 Mio ajoutaient décompression et
redécodage. Dans le profil à 64 nœuds et 32 Mio/store, `Tree.boundaries`
représentait 79,5 % des allocations cumulées. Ce constat n'identifie pas une
boucle infinie ni une duplication de records acquittés.

## Découvrir les frontières sans recalculer les totaux

La version finale réutilise deux structures existantes :

1. **La cellule XOR locale**, calculée par `tree.Cell`. Tester une adresse dans
   cette cellule ne dépend plus d'une nouvelle liste de membres. Pour parcourir
   les préfixes, 17 petits masques suffisent ; les branches incompatibles sont
   écartées. Le prototype `UniformOwner` et l'API intermédiaire `ChildCounts`
   ont été retirés plutôt que conservés comme chemins supplémentaires.
2. **Le catalogue spatial immuable** du stockage. `Store.ZoneInventory` expose
   uniquement les identités de zones et une révision de leur forme. La découverte
   ne lit ni payloads ni sommes de sous-arbres. Les tombstones peuvent laisser
   des candidats conservateurs ; les valeurs vivantes et les permissions sont
   toujours vérifiées au moment de l'export.

Les parents partagent une liste de clés, indexée par **cellule locale + révision
spatiale + collection**. Une mutation de valeur dans une zone existante ou
un changement de membership qui ne modifie pas cette cellule ne reconstruit
plus la liste. Une seule liste est retenue, avec un plafond conservateur de
1 Mio/vnode (`Tree.ExportCacheBytes`). Une liste trop grande est calculée mais
non retenue : le travail froid et le pic de construction restent une limite.

Le mutex d'inventaire coalesce les découvertes concurrentes. Le chemin
`OnLocalWrite` ne l'utilise pas. Les parents destinataires sont filtrés avec la
vue courante ; les valeurs de contributions ne sont pas mises en cache ici.
Les mises à jour reçues pendant la construction froide du catalogue sont
coalescées par zone : conserver la dernière présence suffit, sans accumuler
l'historique de toutes les mutations.

## Retirer le travail global du chemin d'ACK

Après l'écriture durable, `Node.MeshData` rafraîchissait encore la vue globale,
réenregistrait les contacts et pouvait réarmer toutes les frontières de résumé
sous `Tree.Mu` avant de rendre l'ACK. Une écriture locale fournit désormais
seulement sa cellule courante au marquage du résumé. La maintenance normale
continue à actualiser la vue et à propager les totaux. Les RPC de contrôle ne
reconstruisent plus non plus une vue inchangée. Une vraie activation conserve
encore son rafraîchissement et son fold synchrones : leur coût reste à mesurer
et à découpler avant de revendiquer un chemin intégralement indépendant de N.

Un test conserve volontairement une vue de résumé périmée et verrouille
`Tree.Mu` : une écriture durable doit néanmoins recevoir son ACK. Les tests de
lecture continue pendant joins et partition vérifient séparément qu'une lecture
réussie ne perd pas les ACK antérieurs.

## Transfert : moins de commits, préparation hors verrou

L'intégration de FLIP-ONE regroupe fence et cession en **un commit source**, puis
dernier delta, certification et installation en **un commit receveur**. Le cas
normal utilise un seul aller-retour d'installation ; une divergence du staging
provoque un repli vérifié sur le snapshot fencé. Les snapshots et les preuves
restent durables pour reprendre après crash ou perte de réponse.

La source prépare snapshots, empreintes et encodage sur une vue MVCC, hors
`writeMu`. Le commit revalide séquence et état de chaque zone avant de fencer le
lot atomiquement. Trois conflits provoquent un refus réessayable, jamais une
relecture lourde de secours sous le verrou. Pour éviter qu'un flux chaud
n'invalide indéfiniment cette préparation, une barrière volatile met en attente
les nouvelles écritures **du seul lot** pendant sa préparation. Chaque attente
est annulable et bornée à 500 ms ; ce plafond n'est pas une promesse de succès.
Les autres zones continuent. Dès que le droit est cédé, la source relaie vers
le receveur, même si sa réponse d'installation a été perdue.

Le receveur prépare le staging final et les scellés hors writer, puis revalide
l'en-tête, la séquence miroir et l'état dans sa transaction. Les tests injectent
une écriture entre préparation et commit, un remplacement de staging à même
séquence et deux opérations entrantes distinctes. **L'installation finale lit
encore les lignes certifiées et les anciennes destinations pour la comptabilité
sous writer** : l'objectif « aucune lecture de données sous writer » n'est donc
pas encore entièrement atteint.

Les réessais d'installation sont déclenchés par l'échec dans le même worker :
un essai immédiat puis recul borné, pendant 1,5 s au total. La maintenance
reste le filet suivant ; aucun timeout ne restitue un droit cédé. Un receveur
indisponible après la fence peut encore prolonger le gel. Le budget atomique
en octets du **dernier delta** reste à ajouter ; la taille du lot copié ne
constitue pas à elle seule cette garantie.

## Routage et petites suppressions

Une réservation entrante non installée, y compris en phase Ready, renvoie vers
son donneur. Le routage d'écriture n'énumère plus tous les membres en cas de
refus. Il utilise les indices d'ACK, le détenteur connu et les renvois de cession,
avec le détenteur initial comme ancre de secours. Ce n'est pas Kademlia : une
longue chaîne de cessions ou une ancre indisponible reste une limite. Les petits
réessais ne se multiplient pas à chaque saut.

`CollectTransferred` n'extrait plus les valeurs des snapshots qui seront
supprimées après reçu d'installation ; il énumère leurs clés. Les 64 bornes de
lecture des enfants partagent un buffer à tranches non chevauchantes plutôt
que 64 allocations. Les données nécessaires aux lectures restent lues.

## Une course entre les timers de résumés

La suite complète a aussi révélé une coalescence irrégulière : 64 valeurs
partaient en sept petits appels sur environ quatre secondes. Le timer des
marques locales restait actif après leur drainage ; il pouvait déclencher
prématurément la vague suivante. Un callback de flush déjà expiré pouvait
également effacer le timer qui l'avait remplacé.

Le drainage annule maintenant le timer associé et chaque callback vérifie son
identité sous le mutex qui le protège. Un test livre volontairement l'ancien
callback après création de la nouvelle vague ; il ne doit ni demander un flush
ni remplacer le timer courant. Les fenêtres et les assertions du test de batch
restent inchangées. Ce défaut ajoutait du travail inutile ; son poids dans les
74 Gio historiques n'est pas isolé par le profil.

## Mesures et périmètre des conclusions

Les nouveaux compteurs distinguent attente et tenue de `writeMu` par catégorie :
écriture, fence, montage, contrôle et collecte. Ils comptent les transactions
qui changent la séquence du moteur, **pas les fsyncs physiques** ; l'ingestion bulk
utilise une autre admission et est exclue. Le labo et `Node.CommitStats`
exposent ces histogrammes.

La référence statique à charge agrégée constante de 40 écritures/s donne un
p99 d'environ 44–62 ms de 4 à 64 nœuds. Une vérification intermédiaire sur le
même hôte donne 51,4 ms pour l'ancien binaire à 64 nœuds, puis 54,0 ms pour
`cd3f2c1`. Cela ne constitue ni un essai AWS à 16k/s, ni une preuve d'une borne
universelle. Les variantes intermédiaires et leurs régressions restent dans les
traces ; elles ne sont pas présentées comme la mesure du code final.

Commandes, binaires identifiés par SHA, profils, ACK individuels et ressources
sont conservés dans `outputs/summary-work-20261125/` du workspace. Une comparaison
valide indique le budget mémoire, le nombre d'ACK et la période mesurée ; elle
écarte saturation CPU et swap sortant. Les allocations cumulées de deux tests
de durées différentes ne sont pas une comparaison normalisée par écriture.
Tous les stores du labo partagent un processus, un GC et un SSD physique.

## Simplification structurelle suivante

Le lot natif par sous-région proposé par Claude pourrait supprimer points
provisoires et consolidation régionale. Ce protocole n'est pas livré par ce
chantier. L'audit de `ZONE-2` distingue écrivain unique et couverture des lectures :
l'ordre parent/enfants ne doit pas être retiré sans modèle couvrant données,
espace vide, aliases, redélégation et totaux complets ou explicitement incomplets.
Les remontées nécessaires restent hors de la simplification des ACK.

## État des étapes du plan de Claude

| Étape | Livré ici | Limite restante |
| --- | --- | --- |
| 0 — Mesurer | Attente/tenue du writer par type, provenance du binaire, traces ACK | Fsync physique par appelant et latence serveur par type/sauts non ajoutés |
| 1 — FLIP-ONE | Fence/cession source et delta/certification/montage receveur regroupés | Repli snapshot ajoute un échange si staging divergent |
| 2 — Hors writer | Préparation MVCC et revalidation locale, admission temporaire du lot | Installation finale/comptabilité lit encore des lignes sous writer |
| 3 — Routage | Pas de balayage global des membres, Ready renvoie son donneur | Ancre initiale et chaînes de succession restent nécessaires |
| 4 — Reprise | Réessai local, reprise des tickets entrants, pas de retour par timeout | Admission atomique du dernier delta en octets et alerte d'âge à terminer |
| 5–7 — Protocole plus petit | Audit de l'ordre parent/enfants et limites documentées | Modèle enrichi, lots natifs, demande de join et activation à concevoir |

Le microbenchmark final de huit records donne 946 ns / 120 octets / huit
allocations par découverte sans rétention, contre **39 ns / zéro allocation**
pour la liste retenue. C'est un coût local d'inventaire, pas un p99 réseau.

Cette passe ne réduit pas encore le nombre total de lignes de production Go :
43 373 à `83cdeaa`, contre environ 44 000 après instrumentation et préparation
optimiste, hors fichiers de tests. Elle retire les chemins intermédiaires de
découverte et des étapes durables, mais ajoute les contrôles de concurrence.
La réduction structurelle attendue vient du lot natif (étape 5) ; elle n'est
pas annoncée comme déjà réalisée.

## Qualification finale : le p99 pendant les joins reste à traiter

Binaire final `fcba39c`, mode direct, maintenance de production active, HTTP
loopback, fsync réel et délai simulé de 1 ms. Le run dynamique démarre avec
8 192 zones, vise 64 vnodes et garde 40 écritures/s agrégées, avec 32 Mio/store.
Les deux périodes mesurées incluent les joins ; aucun test Go ne tourne en
parallèle. Les 32 dispositifs simulés partagent toujours **un seul SSD physique**.

| Mesure dynamique à 64 vnodes | Référence | Cette passe |
| --- | ---: | ---: |
| Durée jusqu'à clôture/audit | 139,96 s | 76,07 s |
| Écritures acquittées pendant cette durée | 5 540 | 2 998 |
| p50 ACK sur toute la charge | 88,95 ms | 101,03 ms |
| p95 ACK | 258,87 ms | 276,14 ms |
| p99 ACK | 419,37 ms | 410,56 ms |
| Allocations cumulées, préparation et audit inclus | environ 259,1 Gio | 41,51 Gio |
| CPU hôte disponible, minimum échantillonné pendant la charge | 20,12 % | 42,95 % |
| Swap sortant pendant la charge | 0 page | 0 page |

La charge se prolonge jusqu'à la clôture : les comptes d'ACK et la durée
diffèrent. La baisse des allocations n'est donc pas une comparaison normalisée
par écriture, et les 41,51 Gio ne sont pas de la RAM résidente. Le pic RSS
échantillonné pendant la charge est d'environ 740 Mio. La référence complète
dépasse ponctuellement le critère CPU <70 % ; sa fenêtre initiale ci-dessous
le respecte, avec au moins 47,3 % de CPU disponible.

**Sur les mêmes 1 599 arrivées des 40 premières secondes**, le p99 passe de
393,66 à **485,70 ms** : c'est une régression observée, pas un objectif atteint.
Le CPU disponible reste au moins à 64,3 % pour cette passe sur cette fenêtre,
sans swap sortant. Le p50 passe de 109,89 à 107,62 ms et le p95 de 277,51 à
291,82 ms. Répéter le run peut quantifier le bruit ; il ne faut pas attendre
une répétition favorable pour reconnaître ce résultat.

Le contrôle en régime stable à 64 nœuds, 2 Mio/store et 2 048 zones initiales
donne **p50 21,65 / p95 36,82 / p99 55,76 ms**, sur 1 600 ACK, sans échec.
Cela reste dans la plage des contrôles historiques à 64 nœuds, environ
51–63 ms de p99 selon la répétition. Ce seul point ne remplace pas une nouvelle
matrice entière 4–64. Les deux audits finaux trouvent zéro ACK perdu, zéro
ACK dupliqué, zéro double écrivain sondé et zéro gel restant.

### Ce qui reste coûteux

Au dernier échantillon dynamique, les compteurs depuis ouverture enregistrent
2 876 commits d'écriture, **6 946 de contrôle, 1 815 de collecte, 1 071 de
fence et 1 071 de montage**. Ce sont des changements de séquence du moteur,
pas des fsyncs physiques. La préparation des données hors writer n'enlève pas
ce coût fixe de protocole et de clôture.

| Type | Tenue moyenne de `writeMu` | Borne supérieure du bucket p99 |
| --- | ---: | ---: |
| Écriture | 79,44 ms | 262,144 ms |
| Fence | 34,80 ms | 131,072 ms |
| Montage | 92,85 ms | 262,144 ms |
| Contrôle | 30,26 ms | 131,072 ms |
| Collecte | 32,49 ms | 131,072 ms |

L'attente moyenne du writer pour les écritures est de 8,08 ms, tandis que sa
tenue moyenne atteint 79,44 ms. Le temps de validation avant commit, tous types
confondus, vaut 1,16 ms en moyenne ; ces populations différentes ne doivent pas
être soustraites pour attribuer artificiellement le reste aux seuls fsyncs.
Les écritures du moteur, les flushes/merges, la durabilité et les lectures
résiduelles d'installation demandent encore une attribution plus fine.

Les dispositifs comptent 22 427 appels Sync sur le **test entier**, contre
35 242 avant, avec une tenue moyenne incluant le délai simulé de 18,60 ms
contre 17,15 ms. Le contrôle statique mesure 8,28 ms. CPU/RAM disponibles ne
prouvent donc pas l'absence de contention du SSD partagé ; une barrière durable
est un autre coût que le débit réseau ou le débit séquentiel du disque.

Le profil d'allocation final se déplace vers `memChunkIter.refill` (18,17 %
des octets à plat), `Snapshot.spans` (8,63 %) et les lectures réelles de sommes
(`zoneTotal`, 18,46 % cumulés). Les pourcentages cumulés se recouvrent et ne
s'additionnent pas. Les remontées hiérarchiques restent actives.

La fenêtre fence-return→installation conserve un bucket p99 à 524,288 ms ;
sa médiane descend d'un bucket 262,144 à 131,072 ms. Ce sont des histogrammes
logarithmiques, pas des percentiles exacts. La cible p99 sous 100 ms et la
cible de latence pendant joins sous deux fois le régime stable **ne sont pas
atteintes**. Le prochain travail structurel doit réduire les commits autour
du lot et terminer la sortie des lectures d'installation du writer, avec le
modèle de couverture exigé avant de retirer `ZONE-2`.

Validation : `go test ./...`, `go build ./...` et `go vet ./...` réussis. Les
régressions source/montage/crash/admission, les reprises de réseau, la suite
summary et la lecture continue pendant joins/partition passent sous `-race`.
Les assertions de batching du test d'intégration sont conservées. Aucun nouvel
essai AWS n'a été exécuté.
