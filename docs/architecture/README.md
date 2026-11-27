# Architecture du nœud — documents de référence

**Statut : transition architecturale, 25 novembre 2026.**
Le [contrat cible local](../protocol/local-permissions.md) remplace l'autorité
connue globalement par des droits installés, des tickets et une membership de
routage compacte. La [qualification](../protocol/local-permissions-verification.md)
distingue les noyaux purs des campagnes locales finales de stockage et HTTP, vertes au checkpoint `e65dfd3`, avec leurs limites. Les
[passes suivantes](../benchmarks/cleanup-scale-20261125.md) distinguent les
correctifs actuels de ces résultats historiques.
Les pages marquées « prédécesseur » gardent la carte des anciens chemins et leurs
mesures ; elles ne sont pas une option legacy du nouveau protocole.

## Les pages

| Page | Ce qu'elle explique |
| --- | --- |
| [stockage-et-verrous.md](stockage-et-verrous.md) | zonelog, lectures MVCC, transactions de stockage, group commit, carte des verrous et leur ordre, formats refusés à l'ouverture |
| [knowledge.md](knowledge.md) | séparer membership et permissions, contrôle asynchrone, archives des mesures Knowledge |
| [transfert-de-zone.md](transfert-de-zone.md) | le join piloté par le receveur, la bascule par lots, la clôture régionale, les endroits qui décident qu'une zone est inscriptible |
| [chemin-ecriture.md](chemin-ecriture.md) | une écriture de bout en bout : entrée, relais, autorité, admission, ACK, réplication et chemin de secours |
| [autoscale.md](autoscale.md) | quand un vnode demande de la capacité, comment elle arrive, pourquoi il attend pendant un transfert |
| [pannes.md](pannes.md) | la matrice des pannes : pour chaque point d'interruption, qui écrit, qui attend, comment on reprend |

## Comment lire une page

Chaque mécanisme est décrit en trois temps, qui ne se confondent pas :

- **Garantie** : ce qui est vrai dans toutes les exécutions couvertes par le
  modèle de panne ([../protocol/failure-model.md](../protocol/failure-model.md)).
  Elle renvoie à une règle nommée du registre.
- **Mécanisme** : comment le code actuel tient la garantie. Il cite les
  fichiers et fonctions ; c'est la partie qui change le plus souvent.
- **Cible ou mesure** : un objectif que le code vise (une cible n'est pas une
  borne), ou un chiffre mesuré, toujours avec le commit, la configuration et la
  commande qui le reproduisent.

## Le registre des règles

[../protocol/invariants.md](../protocol/invariants.md) est le **seul** endroit
où une règle nommée (`AUTH-1`, `STORE-3`, ...) est énoncée.
[../protocol/verification.md](../protocol/verification.md) dit, pour chacune,
quels tests la vérifient et avec quel statut ; un test du dépôt
(`TestVerificationDocMatchesTests`) refuse une ligne qui cite un test absent.
Les pages de ce dossier citent les règles par leur nom et ne les reformulent
pas.

## Statuts des autres documents

Les documents de `docs/protocol/`, `docs/design/` et `docs/research/` portent
en tête un statut :

- **Actuel** : décrit le fonctionnement branché.
- **Proposé** : décrit une évolution encore incomplète ou non branchée.
- **Historique** : explique une décision passée et ce qui l'a remplacée. Il
  reste lisible, mais ne doit pas servir de carte du code.

Quand une page de ce dossier et un document plus ancien se contredisent, la
page de ce dossier fait foi, et le document ancien doit être corrigé ou passé
en historique.

## Coûts et grande échelle

L'[analyse du graphe de contrôle](../design/scaling-and-control-costs.md) distingue
coût local par morceau, propagation réseau, résumés et politiques d'autoscale.
Ses projections ne sont pas des prédictions de p99. Les
[mesures et comptes de lignes](../benchmarks/cleanup-scale-20261125.md) précisent
ce qui a été supprimé, déplacé dans les modèles ou effectivement optimisé.
