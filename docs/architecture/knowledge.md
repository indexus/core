# Membership : trouver les pairs sans déplacer les droits

**Statut : remplacement branché dans le worktree, qualification complète en cours, 25 novembre 2026.**
Le contrat cible est [droits locaux, tickets et membership](../protocol/local-permissions.md).
La [qualification séparée](../protocol/local-permissions-verification.md) indique
ce qui est testé et ce qui attend encore sa qualification complète.

« Knowledge » regroupait autrefois le carnet de pairs et l'histoire des droits.
Le nouveau protocole sépare ces responsabilités : le réseau diffuse seulement
une membership compacte ; une source et un receveur échangent leur autorisation
locale avec les octets du morceau.

La membership répond « à quelle adresse essayer ? ». Elle conserve le vnode,
son endpoint le plus récent et un éventuel retrait avec renvoi. Recevoir ce
carnet n'autorise jamais un commit. Le disque cible vérifie son droit installé
et ses fences dans la transaction de l'écriture. Une annonce de départ produite
après fermeture de l'admission et drain complet peut aussi servir de reçu terminal
pour collecter un transfert déjà émis dont l'ACK a été perdu. Ce reçu n'accorde
aucune permission ; un changement d'adresse sur le même disque ne suffit pas.
Voir [les conditions exactes](../protocol/local-permissions.md#après-un-départ--un-reçu-terminal-jamais-un-nouveau-droit).

Le contrôle dispose d'un échange versionné avec reçu par pair. Une référence
inconnue après redémarrage provoque une seule reprise complète du contrôle.
Les données, écritures et étapes gelées ne doivent pas exiger cette référence
ni attendre sa réparation. À saturation du travail asynchrone, on abandonne
une demande de synchronisation ; on ne bloque pas le commit utilisateur.

Le nombre d'identités actives et retirées reste un coût. Le nombre d'anciens
transferts et mouvements physiques ne grossit plus chaque record. Kademlia
n'est pas requis pour obtenir cette séparation : XOR utilise la membership et
les indices d'ACK, tandis que les renvois locaux trouvent les exceptions réelles.

L'[instantané historique](../archive/knowledge-frontier-20261125.md) conserve
l'explication de l'attente de 2 s, les chemins full/ref/delta et les mesures
avant/après. Ces chemins ne sont pas une seconde option à maintenir dans le
protocole cible. Leurs benchmarks ne qualifient pas le nouveau binaire.

## Coût restant et version immuable

Un snapshot changé diffuse encore la membership entière ; préparer même une
référence déclenche actuellement un export et un hash du corps. Ces coûts de
réseau/CPU sont [mesurés séparément](../benchmarks/cleanup-scale-20261125.md).
Le contrôle durable local a, lui, été séparé : une mutation de droit conserve le
digest du blob immuable sans le réencoder. Cela supprime un couplage au nombre
de pairs dans ce commit, pas toute la charge de fond des résumés ou du gossip.
Les [projections](../design/scaling-and-control-costs.md) exposent les deltas,
réparations bornées et inscriptions enfant→parent encore à réaliser.
