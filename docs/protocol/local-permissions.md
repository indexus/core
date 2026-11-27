# Droits locaux, tickets et membership

**Statut : remplacement branché dans le worktree `codex/authority-frontier-20261124`,
campagne locale finale verte sur `e65dfd3`, 25 novembre 2026.** Le stockage et le protocole
utilisent les droits locaux, les tickets et la membership décrits ici. Le passage
de scénarios ciblés ne signifie pas que toute la qualification finale a passé.
La [matrice de qualification](local-permissions-verification.md) distingue les
preuves obtenues et les campagnes encore en cours. Aucun résultat AWS ancien
ne qualifie ce nouveau binaire.

## Trois questions différentes

Prenons une zone Q qui passe de S à R.

| Question | Réponse conservée | Effet |
| --- | --- | --- |
| Où essayer d'envoyer la requête ? | Membership XOR, adresse actuelle, indice d'un ACK récent | Choisit une destination ; peut être en retard |
| Qui peut acquitter une écriture ? | Droit installé sur le disque et absence de fence sortante | Autorise le commit local |
| Quels octets sont réellement présents ? | Données installées, progression du transfert et reçus durables | Empêche un droit sans données et une lecture vide par erreur |

Une adresse joignable n'est pas un droit d'écrire. Un message de membership ne
monte aucune donnée et ne crée aucun droit. Inversement, R ne doit pas attendre
la diffusion de son adresse à tout le réseau pour servir une zone déjà installée.

## Ce qui reste sur un disque

Un droit natif est un triplet `(identifiant opaque, région, propriétaire durable)`.
Le propriétaire lie le vnode à son incarnation de stockage ; changer d'adresse
réseau ne change pas les octets déjà installés. Les droits installés d'un disque
ont des régions disjointes, y compris les adresses où aucune ligne n'existe encore.

Le disque conserve :

- ses droits natifs actuels ;
- un compteur durable monotone ;
- ses transferts encore ouverts ou à collecter, avec leurs points installés ;
- ses renvois de lecture actuels ;
- une membership compacte pour trouver les pairs.

Il ne conserve pas une chaîne de chaque ancien détenteur. Le compteur sert à
nommer les nouvelles opérations source, les admissions du receveur et les
coalescences locales. Il n'est ni une horloge murale, ni un vote, ni une époque
globale du cluster.

Une cession décrit un seul morceau : identité du cluster, opération source unique,
droit parent, contacts source et cible, région cédée, ticket, barrière et digest des
données. La source calcule son complément localement. Le message n'énumère pas
l'histoire ni toutes les sous-régions du réseau.

## Reprises, routage et travail disque après l'essai AWS

La [passe de correction du 25 novembre](../benchmarks/recovery-cleanup-20261125.md)
sépare les attentes de réseau des transactions locales. Une erreur d'une source
entrante ne doit pas empêcher le même disque de reprendre ses sorties ou de
compacter ses droits locaux. Le découpage du morceau et sa réservation sont un unique commit local : la
coalescence ne peut pas reprendre le morceau entre ces deux décisions.
Les demandes sans progrès attendent avec un délai
croissant borné ; un donneur occupé ne reçoit pas une proposition par droit
annoncé à chaque tour.

Le propriétaire qui acquitte annonce lui-même un **indice de région installée**.
Avant la consolidation native, cet indice est un point ; après, il peut couvrir
le morceau natif et ses adresses encore vides. Une source exclut ses morceaux
sortants. Un défaut de cache réduit l'indice au point acquitté, sans reconstruire
la topologie pour produire cette réponse. Le client conserve les compléments
non concernés lorsqu'il apprend un nouveau morceau. Cet indice peut vieillir :
il ne remplace jamais le contrôle d'autorité sur le disque destinataire.

La clôture régionale est **un échange d'en-têtes et de reçus**, sans seconde copie
des snapshots. Le receveur compare les digests des preuves des points installés ;
il consolide ensuite le droit natif. Des snapshots dans ce message sont refusés.
Les copies et le mirroring restent uniquement dans les lots de données.

La collecte parcourt les clés et longueurs encodées ; elle ne décompresse pas les
valeurs qu'elle va jeter. Les tombstones opaques sont supprimés à partir de leurs
métadonnées. Les lots de staging sont bornés à 256 lignes. Le verrou local de
commit reste nécessaire à l'atomicité des données et des fences. Dans zonelog,
les lectures froides de préparation s'effectuent sur un snapshot ; sa version est
revalidée avant publication, sans boucle optimiste non bornée. Un conflit de
préparation reprend la décision verrouillée sur l'état récent. Les créations de
collections et leurs manifestes durables restent sur le chemin de commit.

Ces mécanismes n'établissent pas une borne universelle du gel : le disque, le
réseau et un receveur indisponible après émission restent déterminants. Ils ne
créent pas de validation globale. Les messages actuels portent des digests et des
identités de stockage, **pas des signatures byzantines** ; le modèle de pairs
honnêtes et de stockage durable reste inchangé.

## Lots : état de l'intégration du 25 novembre

Le chemin nominal `FLIP-ONE-1` utilise `FenceZones` pour persister fence et
preuves du lot en un commit source. Un message `install-zone` porte dernier
delta, sceaux et autorisations ; `MountZones` les vérifie et monte les données
en un commit receveur. Le receveur peut écrire dès ce commit. La source libère
les attentes du lot avant son bookkeeping `AckZoneInstalls`. En cas de miroir
non conforme, elle renvoie le snapshot immuable fencé, avec les mêmes preuves.

La fence est préparée sur une version MVCC hors writer, puis ses séquences et
états sont revérifiés sous `writeMu`. Trois essais au plus repréparent un lot
concurrentement modifié ; un conflit persistant rend « pas prêt », sans fallback
qui relise les payloads sous writer. La préparation du montage est également
déplacée hors verrou, mais les lectures de lignes et la comptabilité pendant
l'installation restent une limite : toute lecture de données sous writer n'a
pas encore disparu.

Une porte locale arrête les nouvelles entrées du lot avant préparation tant que
le disque reste détenteur ; les écritures déjà admises terminent. Cela évite la
famine de préparation sous charge. Une zone déjà cédée est relayée directement,
sans attendre cette porte. L'attente est bornée et annulable par tentative ;
elle ne donne aucun droit et ne remplace pas la fence durable. Mesurer cette
attente dès la préparation, séparément de fence→installation.

Le stockage publie aussi une admission volatile sur ces seules zones, puis
draine le writer déjà actif avant de prendre sa vue MVCC. Les appels `Apply`
directs et les callbacks déjà en file revérifient cette admission ; un refus
transitoire ne fait pas échouer les autres zones du même groupe de commit.
Cette porte disparaît en sortie, sans modifier l'autorité durable.

Une installation échouée est réessayée dans le même worker, avec recul borné et
une enveloppe de 1,5 s par appel de reprise. Le rejeu réutilise la même preuve ;
le timeout ne restaure jamais le droit source. La reprise durable reste
nécessaire après épuisement de cette enveloppe. Le dernier delta n'a pas encore
une admission atomique par budget d'octets à la fence. Les histogrammes par type
de transaction mesurent le writer et les commits logiques, pas le nombre exact
de fsyncs physiques ; l'admission bulk séparée n'y est pas incluse.

La première activation conserve encore son rafraîchissement de contacts et son
fold synchrones. Il n'y a pas d'accord global pour valider la cession, mais
l'objectif de supprimer tout travail proportionnel au réseau après la fence
n'est donc pas intégralement atteint. Les vues de contrôle inchangées ne sont
plus reconstruites. Voir la [qualification de cette passe](../benchmarks/summary-work-20261125.md).

Le remplacement des
points provisoires par des lots natifs (étape 5) n'est pas livré ; `ZONE-2` et
la clôture régionale restent en place. L'audit préalable ne constitue ni une
nouvelle exécution TLA+ ni une preuve de ce protocole cible.

## Le ticket règle le problème du vieux message

R alloue d'abord un ticket **tentatif**, contenant seulement l'en-tête. La première
copie à chaud confirme ce ticket dans la même transaction que ses premiers octets.
Une copie vide est valide : l'autorité sur l'espace vide doit aussi être transférable.
S persiste la confirmation avant de commencer le moindre gel.

```mermaid
sequenceDiagram
  participant S as S : source
  participant R as R : receveur
  S->>S: réserver opération unique ; rester écrivain
  S->>R: demander admission du morceau
  R->>R: ticket tentatif durable
  R-->>S: ticket
  S->>R: première copie à chaud + ticket
  R->>R: confirmer ticket et octets atomiquement
  R-->>S: confirmation durable
  S->>S: persister confirmation
  loop petits lots, descendants avant parent
    S->>R: copie à chaud et rattrapage
    S->>S: fence durable du lot
    S->>R: dernier delta, sceau et autorisation du lot
    R->>R: installer octets et droit du lot atomiquement
    R-->>S: ACK durable ; R peut déjà écrire ce lot
  end
  S->>S: clôture régionale ; consommer le parent
  S->>R: cession régionale du même morceau et ticket
  R->>R: installer droit natif, conserver les ACK des points déjà installés
  R-->>S: ACK ; collecte source ensuite
```

Un ticket confirmé ne disparaît pas simplement parce qu'un délai est écoulé.
Après la première fence, on reprend la même opération ; on ne rouvre pas S sur
un timeout. Avant cette première fence, S peut annuler durablement, puis demander
à R de fermer son admission. Ce seuil est plus tôt que l'émission régionale :
un point peut déjà appartenir à R alors que le certificat final n'existe pas encore.

## Rejeu et oubli sans registre historique

Après installation, le slot du ticket peut être supprimé. Le compteur durable,
lui, reste. Un ancien paquet dont le ticket a déjà été alloué, sans slot exact
encore ouvert, reçoit un ACK de fermeture **sans remonter aucun octet ni droit**.
Cet ACK est sûr parce qu'une cession valablement émise avait nécessairement son
slot confirmé, conservé jusqu'à installation. Une annulation autorisée précède
la première fence et ne peut produire cette cession.

Un vieux message d'admission sans ticket peut créer un nouveau slot tentatif.
Il ne réactive rien : les anciens paquets gardent leur ancien numéro, et S ne
rebranche jamais une opération déjà émise sur un nouveau ticket. Les slots
seulement tentatifs peuvent être abandonnés sous pression ou au redémarrage.
Le budget d'admission limite cette pression : le code fixe actuellement
`MaxReceiverAdmissions = 128` admissions entrantes non terminées par disque.
À saturation, seul un slot tentatif peut être évincé ; si tous sont confirmés,
on refuse une admission supplémentaire avant sa fence. Ce budget local n'est
pas une limite de 128 vnodes ou machines dans le réseau. Les slots confirmés
doivent être repris jusqu'à résolution.

Les points d'un lot portent le même ticket et l'identifiant stable du futur droit
natif, plus la clé canonique complète de zone. La consolidation régionale ne
recopie pas par-dessus les écritures déjà acquittées sur ces points. Un point
provisoire ne sert pas de parent à une nouvelle sous-délégation avant sa
consolidation native.

Quand deux droits natifs sont des frères exacts et appartiennent au même disque,
ce disque peut les consommer tous les deux pour créer leur parent sous un nouvel
identifiant. Cette coalescence n'exige ni accord réseau ni historique conservé.
Un droit épinglé par un transfert ne peut pas être coalescé à son insu.
`CoalesceLocalRights` prépare son plan hors du verrou d'écriture, puis compare
la version de contrôle au commit. Si une opération concurrente l'a changée,
la maintenance reprend à un prochain tick ; elle n'attend pas une convergence
réseau pour finir sa transaction.

## Membership compacte, convergence et destinations futures

La membership conserve un record par identité : descripteur vnode immuable,
endpoint courant à numéro croissant et éventuel retrait irréversible avec renvoi.
Le numéro d'endpoint est local au vnode. Un numéro plus récent remplace l'ancien ;
deux endpoints différents au même numéro retenu sont contradictoires.

Les annonces honnêtes compatibles convergent malgré ordre inversé et duplicatas.
Ce n'est pas une résolution universelle de forks : les anciens numéros oubliés
ne peuvent plus être audités, et des retraits cycliques contradictoires sont
refusés. Le producteur ferme son admission et transfère ses droits et renvois
avant d'annoncer sa retraite. Les noms et IDs retirés ne sont pas réutilisés.

Lors d'un déplacement physique A→B→A, R peut servir un morceau avant la publication
de la nouvelle adresse du vnode entier. Ses ACK portent l'endpoint futur attendu.
Tant que la membership connaît un numéro inférieur, l'entrée conserve cet indice
direct ; elle ne le renvoie pas vers le B encore publié. Après rattrapage, la
membership résout normalement les anciens contacts. Aucune de ces décisions
ne modifie les droits sur disque.

Le test déterministe à 64 caches applique 768 mouvements et 32 768 livraisons,
puis retrouve exactement 64 records par cache ; le delta JSON mesure 9 348 octets
dans cette configuration. Ce chiffre décrit ces noms et adresses de test, sans
borne générale de taille ni prédiction réseau. Les identités retirées, les vrais
fragments et les opérations non résolues restent des coûts explicites.

## Après un départ : un reçu terminal, jamais un nouveau droit

Un cas doit se fermer même si R a disparu de son ancienne adresse : S a émis,
R a installé, la réponse s'est perdue, puis R a migré vers P ou s'est retiré.
S ne peut plus demander à P de consulter un ticket appartenant au disque R.
P conserve ses propres compteurs, pas l'ancien namespace d'admission de R.

Une annonce compacte de départ peut alors servir de **reçu terminal de collecte**.
Pour une source **déjà `Issued`**, `ReceiverDrained(target)` accepte :

- un MOVE dont l'endpoint courant a un numéro strictement supérieur à celui
  de la cible et un propriétaire de stockage différent ;
- un retrait irréversible dont le numéro atteint au moins celui de la cible.

Le producteur honnête a fermé son admission, résolu toutes ses admissions
confirmées, transféré ses droits et ses renvois, puis seulement publié son départ.
Une opération déjà émise chez S ne pouvait plus être annulée : son admission
chez R a donc nécessairement installé. S peut terminer son ancien transfert et
collecter sa copie. Cela n'installe aucun droit chez S ou P, n'autorise aucune
écriture et ne nécessite ni vote global ni téléchargement de l'histoire.

**L'intake de l'ancien endpoint reste fermé.** Après un MOVE publié, une nouvelle
admission visant cet ancien propriétaire et une ancienne époque est refusée,
même si le champ temporaire `Moving` a été effacé. La confirmation d'un slot
seulement tentatif revérifie ce point. Un retour physique sous une époque plus
récente reste possible ; les anciens Install continuent de consulter leur ticket
fermé sans monter d'octets. Sans cette règle durable, un départ ne prouverait rien
sur des tickets admis après lui et la collecte pourrait perdre des données.

Un simple changement d'adresse sur **le même disque**, même à numéro supérieur,
ne prouve aucun drain. S doit envoyer l'ancien Install à la nouvelle adresse,
avec le ticket d'origine. Si R revient sur son disque d'origine (A→B→A), le
propriétaire courant est de nouveau identique : on reste conservateur et on
consulte ce compteur durable, sans transformer le retour en preuve de clôture.

La membership apporte donc deux informations séparées : des indices de route et,
uniquement pour ces annonces terminales produites sous les préconditions locales,
un reçu autorisant l'oubli d'un backup émis. Elle ne diffuse toujours ni grants
inscriptibles ni graphe de certificats. Le [modèle de départ](../../spec/handoff/ReceiptAfterDeparture.tla)
et ses contrôles négatifs isolent cette nouvelle hypothèse de confiance.

## Lectures : une région inconnue n'est pas une région vide

Une lecture fraîche demande la couverture réellement disponible. Chaque page
rapporte, dans le même snapshot MVCC que les lignes, ses régions installées
**y compris vides**, ses sources encore nécessaires et ses successeurs.

L'entrée commence par les destinations suggérées par XOR, suit les renvois
obligatoires et découvre d'autres pairs si une partie de la région reste inconnue.
Elle réussit seulement après couverture complète et résolution des renvois requis.
Un pair indisponible ou une lacune de couverture donne une attente ou une erreur,
jamais un total partiel présenté comme exact. Les révisions des lignes et les
contrôles de changement de snapshot préservent les écritures acquittées avant
le début de la lecture. La fraîcheur des résumés dérivés est une question séparée.

Les renvois portent les exceptions actuelles, dont les délégations d'espace vide.
Ils migrent avec la responsabilité lors d'un déplacement ; ils ne dépendent pas
de la rétention des anciens reçus de transfert.

## Résumés dérivés : ne pas confondre placement et données installées

Deux joins démarrés avec des vues différentes peuvent laisser provisoirement un
droit chez W alors que XOR désigne T. C'est un état autorisé : le transfert local
reste sûr et une lecture fraîche suit les permissions et les renvois. En revanche,
un résumé construit seulement depuis le placement ne doit pas présenter la copie
conservée chez T comme un total désormais stable. Le même problème peut remonter
à un parent dont la propre cellule native est pourtant complète.

L'accélérateur vérifie donc que ses droits natifs couvrent exactement sa cellule
attendue et qu'aucun transfert local inachevé ne la rend incertaine. Ce calcul est
mis en cache par version de contrôle local et digest de routage ; les écritures
ordinaires ne reconstruisent pas la membership à chaque lecture. Un statut de
poll annonce l'indisponibilité **locale** de cet accélérateur ; les contributions
portent séparément leur éventuelle incomplétude héritée des descendants.

Un ancien total ne devient pas un zéro parce que sa frontière a disparu d'une
énumération. Le parent le marque incomplet et demande cette ancienne clé
explicitement : le détenteur attendu peut répondre un zéro complet versionné,
ou maintenir l'incomplétude. Les demandes sont regroupées par collection, au plus
512 clés à la fois. Les changements de complétude réarment aussi les exports
existants. Après stabilisation, les feuilles complètes réparent leurs ancêtres ;
les anciens endpoints sortis de la membership cessent d'être des dépendances.

Le statut précède les valeurs exportées dans le flux `(SummaryEpoch, Revision)`
du producteur. Le parent refuse un ancien message complet antérieur au dernier
statut observé, même si ce message arrive après l'invalidation. `SummaryEpoch`
survit au redémarrage du disque ; la révision en mémoire recommence seulement
dans cette nouvelle incarnation. Cela n'utilise ni époque globale ni historique
d'autorité, et ne donne aucun droit d'écriture.

En cas d'incomplétude, les lectures ordinaires et les agrégats utilisent le chemin
Query existant. Cela ne rend pas un résumé asynchrone linéarisable : une lecture
ordinaire peut encore observer le retard normal de propagation. `ReadFresh`/Query
et les politiques de cache explicitement périmé restent des contrats distincts.
Cette invalidation évite surtout qu'un ancien total reste faussement stable après
un déplacement hors placement. Elle n'ajoute aucune barrière aux écritures ou
aux fences ; elle peut temporairement réduire le bénéfice du cache de lecture.

## Un répertoire courant, pas les anciennes dettes de transfert

Le disque conserve les destinations régionales encore utiles aux lectures, séparées
de ses permissions. La première copie confirmée indique la source courante ;
l'émission indique le successeur ; l'installation native retire ce renvoi pour
la région désormais couverte localement. Une copie seulement tentative ne change
pas ce répertoire. Annuler une copie chaude ne rend pas son receveur propriétaire :
il garde le renvoi vers la vraie source, même après suppression du slot annulé.

Une ancienne opération `Issued` dont la réponse s'est perdue reste une dette de
collecte, pas une seconde destination régionale obligatoire. Sinon un retour
S→R→C→S imposerait encore de joindre R alors que S possède déjà les nouveaux
droits et toutes les données. De même, une ancienne fence n'interdit que son
`Input.ID` consommé : elle ne gèle pas un nouveau droit revenu sur la même géométrie.

Les mises à jour du répertoire portent une version locale. Une première copie
ancienne livrée en retard ne doit pas écraser la source confirmée plus récente.
La compaction oublie les différences de version seulement quand aucune opération
locale encore possible ne peut changer leur ordre ; elle peut ensuite regrouper
les frères identiques. Cela borne l'état par les exceptions actuelles et les
opérations encore vivantes, sans conserver les trajets terminés.

## Ce que signifie « gel minimal »

Le gel commence à la fence durable d'un lot et finit quand R peut écrire ce lot.
Le rattrapage chaud, l'admission et la confirmation du ticket précèdent ce seuil.
Les faits à valider après la fence sont locaux au morceau et aux participants ;
aucune validation globale, diffusion de membership ou reconstruction d'ancêtres
ne doit se trouver sur ce chemin.

Le découpage spatial local des droits avant admission doit aussi permettre à un
autre grain de partir pendant qu'un premier est encore entrant. Le grain géométrique vise
512 clés et environ 1 Mio d'après les métadonnées, avec découpage avant la fence.
Il se distingue de l'unité de bascule des données : 16 zones au départ, ajustable
entre 4 et 2 048 selon les retours de durée, avec une cible de 40 ms sans écrivain. Ce sont des
objectifs d'ordonnancement, pas des bornes physiques de gel. La
[qualification locale](../benchmarks/cleanup-scale-20261125.md) distingue les
microbenchmarks de contrôle des latences observées sur disque. Des clés partageant exactement le même identifiant XOR ne se
séparent pas géométriquement ; leur groupe reste indivisible et peut dépasser la
cible en octets. Les points provisoires ne deviennent pas sous-délégables avant
leur consolidation native. L'objectif porte donc sur la taille totale d'une région
spatialement divisible, pas sur une borne physique universelle pour tous les atomes.

Cela supprime une dépendance algorithmique ; cela ne promet pas zéro milliseconde.
Réseau, fsync, taille du dernier delta, ordonnanceur et saturation de R restent
réels. Si R tombe après la décision irréversible, préserver l'écrivain unique
peut prolonger l'indisponibilité jusqu'à sa reprise. Il n'existe pas de timeout
sûr qui rende automatiquement S écrivain à nouveau.

## Confiance et reprise

Le modèle couvre des pairs honnêtes, les duplicatas, les messages anciens, les
partitions temporaires et la reprise après crash d'un disque durable. R fait
confiance à la cession du propriétaire local ; il ne revalide plus une descendance
complète jusqu'à la genèse. C'est un changement explicite de garantie par rapport
au protocole historique, pas une compression cryptographique de la même preuve.
Les digests lient les données ; ils ne sont pas des signatures ou un quorum byzantin.

Le compteur, les droits, les fences et les slots confirmés survivent ensemble au
crash. Restaurer un ancien backup sous la même identité peut réutiliser un ticket
ou réouvrir un droit cédé : il faut une nouvelle incarnation et réacquérir les
droits. La perte définitive de l'unique disque détenteur n'est pas réparée par gossip.

Voir la [qualification](local-permissions-verification.md),
le [modèle de panne](failure-model.md), les [modèles bornés](../../spec/handoff/README.md)
et l'[étape historique de checkpoints](authority-frontier.md).
