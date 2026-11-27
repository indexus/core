# Lire à travers les successeurs locaux

> **Historique : étape précédant les permissions locales.** Les cas de lecture
> restent des régressions à conserver, mais les chaînes MOVE, stamps de placement
> et refus du receveur non publié décrits ci-dessous ne définissent plus le runtime.
> Le [nouveau contrat](local-permissions.md) autorise la lecture d'un droit installé
> avant publication et exige la couverture réelle et les renvois nécessaires.
> Voir [la qualification actuelle](local-permissions-verification.md).

**Statut : implémenté sur authority-frontier, 25 novembre 2026.**
Il s’agit d’un prérequis au retrait de la Knowledge globale, pas du protocole
à membership seule déjà terminé.

## Le cas à conserver

A donne une région à B, puis B la donne à C. Une entrée E n’a pas encore appris
ces changements. C a acquitté une nouvelle écriture.

```text
E connaît A
    │ inventaire compact
    ▼
A : copie éventuellement collectée, pointeur vers B
    │ successeur conservé après compaction des reçus
    ▼
B : copie éventuellement collectée, pointeur vers C
    │ nouveau successeur
    ▼
C : données installées, dont l’écriture acquittée
```

Une lecture ne peut pas s’arrêter après A ou B et annoncer un résultat vide.
Elle suit tous les successeurs indiqués. Un échec réseau à l’un de ces relais
fait échouer la tentative entière ; le résultat n’est jamais validé comme
complet avec les seuls shards disponibles.

## Les pairs nécessaires à une lecture

Un receveur non installé désigne aussi ses sources encore nécessaires : deux
régions entrantes peuvent avoir deux sources distinctes. Une redirection initiale
vers l’une d’elles conserve une référence vers le receveur lui-même pour que sa
lecture déléguée retrouve toutes les sources, avec les points déjà installés.

Pendant le transfert granulaire, les états de zones existants identifient les
receveurs des points déjà cédés. Après la cession régionale, une petite frontière
locale de régions identifie les successeurs. Il n’est pas nécessaire de garder
le certificat historique pour retrouver ces successeurs.

`EmitCausal` écrit le pointeur vers la sortie cédée dans la même transaction que
l’émission irréversible. `InstallCausal` efface les pointeurs à l’intérieur de
la région acquise dans la même transaction que l’installation. Un retour partiel
ne supprime pas les pointeurs de l’autre partie. Les régions sœurs exactes avec
le même successeur fusionnent. Un aller-retour complet répété entre deux disques
ne crée donc pas un reçu par tour. Le test de 30 allers-retours sur vrais
disques contrôle zéro pointeur chez le détenteur, un seul chez l’autre disque,
y compris après réouverture et rejeu d’anciens ACK.

La frontière est une ligne de métadonnées locale, pas un journal d’opérations.
Son remplacement parcourt ses fragments locaux, sous le verrou transactionnel
local existant ; il n’ajoute aucun accord réseau. Ce n’est pas un coût constant
pour un annuaire arbitrairement fragmenté, ni une borne indépendante du nombre
de destinations distinctes conservées. Une coalescence distante n’est pas encore
répercutée automatiquement aux anciens relais. La réparation et la migration
ultérieures devront réduire aussi ces fragments périmés.

## Le gel intermédiaire

Supposons que B ait acquitté une écriture absente de l’ancienne copie de A.
B gèle ensuite son lot pour C, mais C ne l’a pas encore installé.

```text
A : ancienne copie    B : copie gelée contenant l’ACK    C : pas encore installé
                            └── reste lisible ──┘
```

Une requête déléguée doit lire la copie gelée de B et suivre son pointeur vers C.
Ne servir que les zones encore écrivables de B ferait disparaître cet ACK de la
lecture. `relayReadableIn` combine donc le prédicat de lecture existant avec les
points installés avant le certificat régional. Le prédicat d’écriture
`writableIn` ne change pas : lire une copie gelée n’autorise pas à écrire dessus.

## Le chemin réseau

Les RPC d’inventaire ont un seul chemin compact : pas de bundle Knowledge, pas
de réparation globale ni de repli vers un ancien mode sur un défaut de cache.
Les RPC sont uniquement paginés. Une requête sans page, ou qui tente de
porter un bundle ou sa référence, est refusée avant merge. Chaque page fournit les lignes et les pairs nécessaires du **même snapshot
MVCC**. Une réponse relayée ajoute l’identité du receveur à lire directement ;
cette identité ne prétend pas décrire son état installé. Une modification entre deux pages invalide la tentative, comme avant.

Le demandeur garde un ensemble de successeurs déjà interrogés et suit leurs
propres successeurs. Un même disque peut nécessiter une lecture initiale normale
puis une lecture déléguée : avant son installation régionale, il a pu transmettre
la première à sa source, alors qu’il détient déjà des points installés. La lecture
déléguée ne rebondit pas vers cette source. La déduplication borne les revisites ;
elle ne prouve pas à elle seule la couverture d’un annuaire arbitraire.

Un pointeur d’hôte connu comme prédécesseur dans une chaîne MOVE validée
avance vers son endpoint actuel en O(1), via un index immuable. Un futur receveur
absent de cette chaîne n’est jamais ramené vers l’ancien endpoint. Un retour sur un endpoint déjà utilisé porte aussi
l’empreinte du placement connu lors de la cession : tant que ce placement n’a
pas avancé, le pointeur de retour ne peut pas être renvoyé vers son donneur.
Un receveur ayant installé un droit régional mais pas encore activé son placement
physique refuse explicitement une lecture déléguée, au lieu de répondre vide.
Les noms
logiques des vnodes et leurs droits ne changent pas. L’historique MOVE existant
n’est pas compacté par cet index.

Les snapshots de la même zone sont réconciliés par leur séquence, comme avant.
Ce n’est pas un snapshot global des écritures concurrentes entre shards.
Les pointeurs ne donnent aucun droit d’écriture et ne modifient pas la frontière
d’autorité. Le modèle de confiance reste celui de pairs honnêtes avec reprise
sur disque durable ; ce ne sont pas des signatures contre des pairs byzantins.

## Vérifications

`TestQueryFollowsCompactedSuccessorsWithoutGlobalKnowledge` utilise les vrais
services et disques :

- A→B puis B→C, collecte et compaction effective des reçus/certificats ;
- ACK propre à B, lecture quand B est gelé avant installation sur C ;
- ACK propre à C, lecture finale à travers A et B depuis une entrée restée stale ;
- constructeurs Knowledge de tous les participants volontairement verrouillés ;
- aucune Knowledge dans les RPC et aucune modification de la connaissance de E ;
- indisponibilité de C : erreur, aucun résultat partiel.

Trois mutations temporaires du code ont été testées puis retirées : oublier le
pointeur, s’arrêter après un saut, et ignorer la copie gelée intermédiaire. Chacune
fait échouer ce test. Ces contrôles négatifs sont conservés dans les résultats,
pas comme options de production.

`TestQueryFollowsAllSourcesOfUninstalledRegions` vérifie deux régions entrantes
non installées, leurs deux sources, les lectures limitées à chaque préfixe et
la lecture complète. Les tests de déplacement physique contrôlent aussi les
lectures à chaque frontière et l’arrêt effectif de l’ancien endpoint.
`TestQueryNeverReturnsEmptyDuringPhysicalReturnBeforePlacementPublication`
reproduit le retour vers le disque original, entre installation/collecte et
publication du nouveau MOVE. Il échoue sans le marquage du pointeur et la garde
de placement ; une indisponibilité explicite est autorisée dans cette fenêtre.

Un join peut encore demander un parent qu’un donneur a déjà compacté. Sans
réparation incidente par les inventaires, ce cas est maintenant explicite :
si aucune réservation de ce transfert n’a commencé, le join recalcule sa demande
après l’échange pré-fence du checkpoint courant. Il ne réouvre pas une fence
existante. `TestJoinReplansCompactedParentWithoutInventoryRepair` reproduit ce cas
avec un aller-retour réel A→B→A avant le join.

Le harnais de transactions vérifie maintenant, après reprise et compaction,
que les pointeurs régionaux conservés ne forment pas de cycle et rejoignent le
propriétaire réel pour les zones représentées. Les mêmes graines, coupures d’I/O,
crashes et rejeux s’appliquent à ces écritures de répertoire atomiques.

`ReadSuccessors.tla` explore 107 états pour trois disques et des cessions
terminées A→B→C→A→B. Le read démarre après une cession et stabilise les suivantes.
Le modèle contrôle la clôture, la lecture complète et les retours sans cycle.
Trois variantes incorrectes produisent les contre-exemples attendus. Il ne
formalise ni le scan MVCC concurrent, ni l’overlay géométrique Go, ni tout le
futur annuaire régional ; ces cas ont des tests distincts.

## Ce qui reste avant la membership seule

L’entrée choisit encore sa **couverture initiale** depuis sa frontière connue,
et compare son empreinte avant/après la lecture. Le gossip et la préparation
chaude diffusent encore l’autorité actuelle. Les résumés conservent aussi leurs
contrôles d’autorité existants. Cette étape retire les bundles du chemin de
lecture par saut ; elle ne retire pas encore toute connaissance globale.

Le coordinateur XOR d’une région devra ensuite conserver ses délégations et
exceptions, les inclure dans la copie de sa portion (espace vide compris), et
transférer atomiquement cette responsabilité. Il faudra qualifier les totaux,
les retours de régions, les changements de placement et les retraits sans exiger
que tous les anciens relais restent disponibles. À ce stade, une chaîne ancienne
peut toujours nécessiter un ancien hôte : on ne promet pas encore une réparation
à partir de la seule membership.

Le seul format disque est désormais `indexus-flat-3`. Un ancien disque peut
avoir déjà oublié des successeurs ; il est refusé, sans migration implicite.
Les champs de lecture sont `Delegated` et `Referrals`, sans alias pour
`Writable` ou `Receivers`. Cette branche reste
réservée à des binaires homogènes et des répertoires de données compatibles.
Aucun répertoire de production n’a été migré par ces essais.

Voir [frontière d’autorité](authority-frontier.md), [Knowledge](../architecture/knowledge.md),
[lecture](query.md) et [qualification](handoff-reliability.md).
