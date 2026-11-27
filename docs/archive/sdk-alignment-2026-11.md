# Toutes les expériences sur le SDK Indexus

Les quatre applications utilisent désormais `js-indexus-sdk` pour leurs données :

| Application | Lecture de données |
| --- | --- |
| Real Estate | `Network.getSets/getSet`, avec `getContentRecords` pour les contenus terminaux |
| Drawing | `Network.getContentRecords`, ou `getSets` pour le dessin progressif |
| Weather | `ContentAssets` → `Network.getContentRecords` pour tuiles, atmosphère, vent et villes ; catalogue via le même Network |
| 3D | `ContentAssets` → `Network.getContentRecords` pour LiDAR, images, acquisitions, empreintes et contexte de reconstruction |

Le proxy `/api/p2p` transporte le protocole tel quel, comme pour le dashboard.
Il ne convertit plus des URLs de fichiers en requêtes Indexus. Les seuls fetch
applicatifs directs restants sont la configuration/authentification de l’API et
le masque France de référence. Les fonds de carte OSM/MapTiler et leurs
ressources graphiques restent externes.

## Lecteur commun

`ContentAssets`, dans le SDK, reçoit une instance de `Network` et un nom de
collection. Il calcule la position de contenu à partir du chemin canonique,
utilise l’admission et le routage du SDK, vérifie toutes les parties, leur
longueur et leur SHA-256, puis décompresse dans un flux borné. Pas de repli
statique ou filesystem. Les lectures simultanées d’un même bloc sont partagées.
Annuler un consommateur ne pénalise pas ceux qui utilisent encore ce bloc ; le
transport est annulé lorsque le dernier consommateur se retire.

Le cache LRU des octets vérifiés est limité à 8 MiB par contexte. Les copies
retournées à l’appelant ne peuvent pas modifier le cache. Les caches spécialisés
de géométrie et de météo restent bornés séparément. Chaque Network de météo/3D
utilise quatre places de transport ; le budget global du serveur reste commun.
Les payloads et les collections existantes n’ont pas été réimportés.

L’initialisation commune de météo/3D découvre le réseau actif via `/api/health`
et `/api/mesh`, renouvelle le token, puis crée `Network`, comme les autres
expériences. Le SDK importe sa dépendance binaire pour fonctionner aussi dans
les navigateurs et workers sans `Buffer` global.

## Suppressions

- Adaptateur Python, ses tests et le service local sur le port 3867.
- Proxy Vite des URLs de blocs vers cet adaptateur.
- Export/copie des snapshots dans les builds météo/3D.
- Repli vers les anciennes URLs de villes et binaires LiDAR non compressés.

Les anciennes URLs publiques de données sont refusées en développement (410),
même si les fichiers source sont présents dans `public/`. Les builds incluent
l’interface et le masque de référence, pas les jeux de données. Ils nécessitent
un backend Indexus avec les collections appropriées. La production existante
n’a pas été déployée ou modifiée durant cette intervention.

## Vérifications

- 204 tests du SDK, dont adressage, checksum, budgets, cache, déduplication,
  annulation indépendante et fonctionnement sans Buffer global.
- 112 tests météo/3D ; builds SDK, météo, 3D et dashboard.
- Lectures réelles par le SDK des manifestes météo, LiDAR, scènes, orthophotos
  et des cinq lieux, avec l’adaptateur arrêté et son code retiré.
- Lectures SDK natives : racine DVF = 546 278, dessin = 69 enregistrements.
- Chrome : scène Lyon reconstruite, état « Prêt », 2 098 806 observations ;
  météo, villes, nuages et passage du 28 au 29 janvier fonctionnels.

Artefacts locaux : `.local/sdk-alignment/`. Les logs de tests et builds sont
également conservés dans les répertoires des deux applications et du SDK.


L’organisation des points d’entrée et de la connexion partagée est décrite dans
[Organisation des expériences](experience-organization.md).
