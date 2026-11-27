# Noyau actif : responsabilités et contrats minimaux

**Statut : historique.** Cette revue cite `internal/cluster` et Bolt, qui n'existent plus ; les responsabilités sont aujourd'hui dans `storage` et zonelog. Le fonctionnement actuel est décrit dans [README.md](../architecture/README.md).

Revue locale du 2 novembre 2026. L'objectif est de réduire les endroits où une
règle peut diverger, tout en conservant les barrières durables et la reprise P2P.
Le nombre de fichiers ou de lignes ne mesure pas à lui seul cette simplicité.

## Cinq responsabilités

| Couche | Décision possédée | Limite |
| --- | --- | --- |
| `protocol` | géométrie XOR, identité des zones, plans purs | une vue fournie ne prouve pas sa complétude globale |
| `causal` | validation et merge des faits compatibles | connaître un grant n'installe pas ses données |
| `cluster` | transactions, bytes, reçus, séquences, fences et installation | seul ce niveau autorise une nouvelle mutation |
| `mesh` | échanges, handoffs et reprises | les routes et retries ne créent pas de droit supplémentaire |
| `indexus` | API, routage et agrégations dérivées | les résumés ne sont jamais des permissions d'écriture |

Le test d'architecture contrôle les dépendances entre ces couches. Les faits
purs ne font pas d'IO, et seul `cluster` accède à Bolt. L'auditeur central du
banc reste un observateur de test ; il ne valide pas les transitions de production.

## Les notions à ne pas confondre

- Une zone est identifiée par `(collection, chemin)`. Son ID XOR est une adresse,
  qui peut être identique à celle d'un ancêtre ou descendant, et peut valoir zéro.
- `V_active` détermine une géométrie désirée. Un contact connu ou un owner XOR
  ne confère pas de droit sur les bytes.
- La chaîne d'autorité désigne un détenteur ; l'installation locale, la présence
  active et les fences déterminent s'il peut réellement écrire.
- `RoutingSnapshot` est une surface de lecture immutable. Elle n'expose aucune
  installation, aucun handle writable, aucune map de preuves mutable.
- Les résumés sont reconstruisibles à partir des records durables et peuvent
  être en retard pendant une migration.

## Un seul prédicat d'écriture

`internal/cluster/permission.go` contient le prédicat causal utilisé par la
pré-vérification et répété dans la transaction de commit. Il vérifie : activité,
placement du disque, détenteur de la zone, tous les gels sortants, puis
installation du grant ou de l'opération par zone correspondante.

Un ancien flag `Installed` ne peut pas masquer un gel plus récent. Le snapshot
de contrôle provient des octets exacts de la transaction, et aucune décision
« writable » n'est réutilisée entre deux transactions.

Un ACK implique un commit durable dans les modes synchronisés. Une erreur ou
l'absence d'ACK n'implique pas l'absence de commit ; les reçus permettent le retry.
L'invariant reste au plus un applier par zone, avec des fenêtres à zéro writer
entre gel et installation. Les lectures et la reprise sont qualifiées dans
[read-write-phase-verification.md](read-write-phase-verification.md).

## Une implémentation par transition de zone

Les quatre formes unitaires délèguent maintenant aux transactions par lot.
Elles conservent leur API mais ne possèdent plus une autre version des règles.

| Transaction | Effet principal |
| --- | --- |
| `FreezeZones` | capture et pose la fence, après vérification des descendants |
| `StageZones` | remplace atomiquement le staging et certifie les bytes |
| `EmitZoneTransfers` | persiste la preuve irréversible de transfert |
| `InstallZones` | monte bytes et reçus avec le fait local d'installation |

Contrat commun : lot vide sans effet ; erreur dans un élément annule le lot
entier ; aucun état partiel ne survit au rollback. L'installation initiale exige
une preuve liée à la même opération, à la même zone et à sa séquence certifiée.
Les contrôles de capacité sont exécutés avant le commit de certification et
installation. Une copie tardive ne peut pas remplacer la base déjà installée.
Le remplacement du staging supprime puis recrée son bucket dans la même
transaction, au lieu de vider manuellement une liste de champs.

Les flags `Frozen`, `Sealed`, `Transferred`, `Installed` sont des faits locaux
qui diffèrent selon source et destination. Les remplacer par un unique enum
linéaire partagé masquerait ces deux rôles et changerait le format persistant.
Leur sens est défini par les transactions ci-dessus, pas par l'ordre des maps.

Le budget porte sur les clés/valeurs de données, staging et reçus ; il ne constitue
pas une limite de tout le filesystem ni de tout l'historique de preuves de contrôle.
Les nouveaux contrôles sont par lot. Le chemin d'insertion conserve son contrôle
incrémental de capacité ; cette passe ne lui ajoute pas un scan complet.

## Groupement des écritures disque

Un seul coordinateur de group commit tourne par store. Il laisse les nouvelles
écritures s'accumuler pendant le commit précédent, puis traite le groupe suivant
avec une limite de taille. Cela évite de former de nombreux petits lots Bolt
dont les timers expirent pendant qu'ils attendent tous le même verrou disque.
Une courte fenêtre collecte les groupes partiels ; le défaut reste 256 écritures
maximum et 2 ms de collecte. Aucun coordinateur réseau ou validateur global
n'intervient.

Le mutex de la file n'est tenu ni pendant la transaction ni pendant l'attente de
l'ACK. Chaque callback revérifie les permissions dans sa transaction. Les refus
sans mutation, dont un conflit de reçu, ne font pas annuler les autres écritures.
Une erreur après mutation annule le groupe ; les callbacks sains sont rejoués
ensemble et le callback fautif est retenté isolément. Leurs sorties sont remises à
zéro à chaque exécution. L'ACK suit le retour du commit synchronisé, jamais la
simple mise en file. La fermeture refuse les nouveaux appels et termine les
écritures déjà acceptées avant de fermer Bolt.

Les tests contrôlent les lots de 16 et 256 derrière un commit bloqué, l'absence
d'ACK anticipé, la fermeture avec des écritures en attente, la relecture après
réouverture et le rollback d'un callback fautif sans perte des callbacks voisins.
Les écritures de contrôle et les transferts partagent toujours le writer Bolt ;
ce groupement ne prétend pas supprimer toute contention entre ces opérations.

## Lecture et réparation

La lecture emploie un seul résolveur `meshReadRoute`, avant la décision de cache
et après une query qui a pu apprendre un nouveau placement. Le détour qui
rechargeait `CausalState` et reconstruisait toute la réplique a été supprimé.
Le cache publie ensemble la vue de membership et le caractère local de son
placement physique, via un seul pointeur atomique. Le rafraîchissement réutilise
l'instantané durable déjà chargé ; le démarrage ne reconstruit plus une seconde
réplique. Un MOVE entre disques rafraîchit ce cache même sans changement de digest
de membership. La découverte des contacts utilise directement le nom du holder
et l'index des membres, sans rechercher son descripteur dans toute la liste.
Le résumé local ne sert que sur le propriétaire physique courant. Les lectures
profondes distantes utilisent encore l'inventaire durable ; leur coût et leur
disponibilité pendant un transfert restent à optimiser séparément.

Une réparation reçoit une vue capturée avec son digest. Si un delta plus récent
fait refuser un snapshot, le travail reste en attente pour le prochain tick.
Elle ne peut plus publier une base incomplète comme terminée jusqu'au balayage
périodique de cinq minutes. Seule la tentative la plus récemment démarrée peut
publier son résultat : une fin tardive ne peut pas effacer une reprise plus récente.
Une réparation entièrement appliquée cesse de rescanner à chaque tick.

## Nettoyage effectué et limites

Les variantes unitaires/par lot du handoff ont été unifiées et le prédicat
d'autorisation extrait dans un fichier de 79 lignes. Le fichier de transitions
par zone est passé de 632 à moins de 500 lignes, malgré l'ajout des validations
manquantes. Cette réduction concerne du code de règles dupliqué.

La revue a reproduit quatre admissions incorrectes : dépassement de budget à
la certification, dépassement à l'installation, opération étrangère et séquence
sans lien avec les bytes certifiés. Les tests les refusent désormais et contrôlent
le rollback. Un autre contre-exemple montre une copie tardive remplaçant une base
installée ; la transition est maintenant monotone.

La suite complète a aussi trouvé une convergence retardée du résumé sous charge.
Les données durables étaient présentes ; des bases dont le fold était refusé
étaient néanmoins marquées comme réparées. Le délai du test n'a pas été augmenté.
Des tests ciblent maintenant le retry et l'ordre des fins de réparation.

Le noyau actif ne doit pas être élargi tant que ses limites sont ouvertes :
fermeture régionale P2P, contrôles froids non branchés, historique des preuves et
reçus, codec versionné, lectures distantes plus ciblées. La compatibilité legacy
et les modèles expérimentaux restent une surface à séparer progressivement ;
cette passe ne prétend pas les avoir supprimés ni qualifier un scale linéaire.

## Validation de cette passe

La suite fonctionnelle de tous les packages passe avec `go test -race ./...`,
hors `TestScalerActivationReturnsEarlyAndForksStayUnique` et
`TestScalingSpawnsAreDonorOnly`, qui restent des qualifications de débit extrême
séparées. Le premier échec de convergence et les contre-exemples d'admission sont
conservés dans `outputs/indexus-foundations-2026-11-02` à côté du rejeu final réussi.
Les nouveaux tests couvrent les budgets, l'opération et la séquence certifiée,
le rollback des remplacements, les lots vides, les copies tardives et les retries
et fins concurrentes de réparation. Les tests existants des coupes durables,
lectures, joins, MOVE et écritures concurrentes passent aussi. `go vet ./...`,
`gofmt` et `git diff --check` ne signalent aucun problème.
