# Pannes et reprise du transfert par tickets

**Statut : actuel, 25 novembre 2026.** Pairs honnêtes, stockage durable,
crash/reprise et messages perdus, dupliqués ou retardés ; aucune signature
byzantine ni horloge globale n'est supposée. La
[matrice antérieure](../archive/pannes-before-cleanup-20261125.md) est historique.

La source et le receveur persistent une même opération. Le routage donne un
indice ; le disque décide du droit d'écrire. Après la première fence, une perte
de réponse n'autorise jamais à choisir arbitrairement un autre receveur ni à
rouvrir la source.

| Coupure | Qui peut écrire le morceau ? | Reprise et limite |
| --- | --- | --- |
| Avant confirmation du ticket | Source | Le ticket tentatif peut être annulé ; aucune permission cible |
| Copie confirmée, avant première fence | Source | Copie chaude et mirroring reprennent ; une annulation doit être durable et confirmée |
| Fence durable, cible non installée | Personne sur la partie gelée | Reprendre la même décision ; aucun timeout ne rend le droit à la source |
| Installation durable, réponse perdue | Receveur | Répéter l'installation retrouve le reçu ; ne pas écraser ses écritures suivantes |
| Consolidation native après les points | Receveur pour ses points déjà installés | Consolider les droits/reçus, sans seconde copie des données ; l'espace vide suit le droit natif |
| Receveur absent après décision irréversible | Source interdite ; receveur indisponible | Attendre sa reprise ou un reçu terminal honnête répondant au contrat exact ; aucune borne de durée universelle |
| Réparation entrante indépendante en panne | Chaque détenteur conserve ses droits | Les reprises sortantes indépendantes ne doivent pas être sautées à cause de cet échec |
| Réponse de membership perdue ou référence inconnue | Inchangé | Réparer le carnet de pairs séparément ; aucune attente de validation globale pour un morceau installé |
| Ancien reçu rejoué après collecte et retour | Détenteur du nouveau droit installé | Le ticket fermé répond sans remonter les anciennes données ni réactiver un droit consommé |

## Lectures et résumés

Une lecture fraîche suit les sources/successeurs nécessaires avec la couverture
de la région, y compris son espace vide. Un pair obligatoire absent ou un trou ne
se transforme pas en résultat partiel présenté comme complet. Les données déjà
acquittées ne doivent pas disparaître d'une lecture commencée après l'ACK.

Les contributions de résumés sont versionnées et vérifiées contre le détenteur
courant. Les lignes locales et leurs corrections se lisent dans une version
MVCC cohérente ; conserver les lignes cédées jusqu'à leur collecte ne doit pas
compter leur contribution deux fois.

Une annulation de polling est un échec de réparation, pas un certificat de
complétude. Le tour suivant commence après les pairs réellement tentés, pour ne
pas affamer les autres. Un export ou passage complet trop long demande encore
une réparation reprenable par pages/version ; la deadline ne résout pas cette
obligation de progression à elle seule.

## Disque et observabilité

Une erreur de lecture bloque la réussite du commit ou de la lecture concernée ;
elle ne signifie pas « clé absente ». Les digests de membership sont vérifiés au
chargement à froid et à la réouverture ; les versions internes en cache sont
immuables. Une mutation locale de droit réutilise leur référence sans réencoder
tous les membres du réseau.

Suivre par opération : admission, fence, émission, installation, premier ACK,
route directe, clôture et collecte. Un transfert ouvert peut encore copier ; un
transfert gelé est une information plus précise. Une latence maximale d'écriture
ne désigne pas à elle seule une phase ou un mutex responsable.

Les scénarios exécutés, modèles bornés et limites sont dans
[la qualification](../protocol/local-permissions-verification.md) et
[la passe de contrôle](../benchmarks/cleanup-scale-20261125.md).
Le [contrat détaillé](../protocol/local-permissions.md) définit notamment les
conditions strictes du reçu terminal après départ.
