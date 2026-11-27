# Toute la pile en local : un nœud, la gateway, Explore

Un seul lanceur, `scripts/local/explore.sh`, construit les binaires, démarre
l'issuer et **un** nœud Indexus, construit puis installe toutes les
collections qu'Explore lit (scène d'Anglet en 3D, météo AROME, immobilier
DVF 2020), puis démarre la gateway du dashboard et Explore. Les mots en
*italique* sont dans le [glossaire](glossary.md).

Pour démarrer avec les données déjà préparées et activer la délégation vers
des vnodes supplémentaires, utilisez le [démarrage Docker](../quickstart/README.md).

## Prérequis

| Outil | Pour |
| --- | --- |
| Go 1.26 | `node`, `issuer`, `localdata`, `prep` |
| Node 22 | la gateway (`dashboard/server.js`) et Vite (Explore) |
| Python 3.12 + numpy, Pillow, laspy[lazrs] (`tools/prep/requirements.txt`, le seul) | les constructeurs de `tools/prep` (LiDAR, scènes, météo, commune) ; HTTP et Lambert-93 sans requests ni pyproj |
| Outils en ligne de commande Xcode (`xcrun swiftc`, Metal) | les noyaux GPU de `tools/prep/metal` (macOS) |
| curl | les attentes de santé du lanceur |

## Disposition des dossiers

Les quatre dépôts sont frères. Le lanceur tourne depuis une copie dédiée,
`_run/`, pour que le travail en cours dans les dépôts ne change pas la pile
lancée :

```text
indexus/
  core/ sdk-js/ dashboard/ experiences/             dépôts de travail (node_modules installés)
  _run/core/ _run/sdk-js/ _run/dashboard/ _run/experiences/   worktrees lancés (git worktree)
  _run/.data/                                    tout l'état de la pile (voir plus bas)
```

```sh
cd indexus
for r in core sdk-js dashboard experiences; do git -C $r worktree add ../_run/$r; done
```

Chaque dépôt de travail a ses paquets installés (`npm ci` dans `sdk-js` et
`experiences`, `pnpm install` dans `dashboard`) ; dans `sdk-js`, `npm ci`
construit aussi `dist/`, que la gateway charge.

Les worktrees de `_run` réutilisent les `node_modules` des dépôts de travail
par un lien relatif (`_run/experiences/node_modules -> ../../experiences/node_modules`)
que le lanceur crée s'il manque ; il relie de même un `.env.local` (clé du
fond de carte). `EXPLORE_ORIGIN` désigne un autre dossier de dépôts,
`EXPLORE_DATA` un autre dossier d'état.

## Entrées

Le lanceur construit les *batches* à partir de fichiers qui ne sont pas
dans les dépôts :

| Entrée | Produit par | Pour |
| --- | --- | --- |
| `experiences/data/world/anglet-1km/`, `experiences/public/world/anglet-1km/{points-raw,points-imagery}` | `npm run prepare:world` dans `experiences` (téléchargements IGN) | végétation, sol, houppiers |
| `_run/.data/collections/{dvf-2020,ortho,buildings-v2,buildings-v2-fine}.ndjson` | `experiences/scripts/world` (orthophoto, bâtiments) et l'export DVF | `DvFMV2020idx0001`, `OrthoBDORT2024v1`, `BuildLidHD2025v2`/`f2` |
| `_run/.data/launch/weather-ortho.ndjson` | export de l'ancien contenu météo (`localdata -export`) | les collections météo |

Une entrée absente est signalée (`absent : …`) et sa collection sautée ;
le reste de la pile démarre.

## Lancer, arrêter

```sh
cd indexus/_run/core
scripts/local/explore.sh        # lance tout, ou reprend ce qui manque
scripts/local/explore.sh stop   # arrête tout ; les données restent
```

Le lanceur est idempotent : relancé, il garde les services vivants, saute
les batches inchangées et s'arrête au premier problème avec un message
(port occupé par un autre processus, import refusé…).

Étapes :

1. outils (Go 1.26, Node, curl) et liens `node_modules` ;
2. construction de `node`, `issuer`, `localdata` dans `_run/.data/bin`
   (un `node.prebuilt` posé à côté est pris tel quel) ;
3. issuer (22030) et nœud (`init` la première fois, puis `serve`, borné à
   4 cœurs et 1 Gio) ;
4. `scripts/local/prep-all.sh` construit une batch par collection dans
   `_run/.data/batches/`, puis chacune est installée d'un coup par
   `localdata -bulk` (*bulk ingest*) ;
5. gateway (3847) et Explore (5174) ;
6. la végétation, la plus grosse batch, en dernier : Explore est déjà ouvert.

## Ce qui tourne où

| Service | Adresse |
| --- | --- |
| Explore, Anglet, météo | http://127.0.0.1:5174/explore.html?scene=anglet |
| Explore, Anglet, immobilier | http://127.0.0.1:5174/explore.html?scene=anglet&data=realestate |
| Gateway (`/api`) | http://127.0.0.1:3847/api/mesh |
| Nœud (p2p / monitoring) | 21031 / 19031 |
| Issuer | 22030 |

Réseau `indexus-explore-local`, déclaré à la gateway par
`_run/.data/networks.json`. Le dashboard d'administration (5173) n'est pas
lancé ; au besoin : `cd _run/dashboard && node node_modules/vite/bin/vite.js --host 127.0.0.1 --port 5173`
(il passe par la même gateway).

## Collections

`prep-all.sh` construit (les collections d'une scène sont nommées par son entrée de `experiences/scripts/world/catalogue.json` ; détail et mesures : [ingest/pipeline.md](ingest/pipeline.md), section 6) :

| Collection | Contenu |
| --- | --- |
| `SceneExplo2026v1` | une scène par record, à son origine ; nomme ses collections |
| `OrthoBDORT2024v1` | orthophoto, tuiles de l'espace `gps` |
| `TerraLidHD2025v1` | sol LiDAR HD, niveaux 13 à 19 |
| `BuildLidHD2025v2`, `BuildLidHD2025f2` | bâtiments (toits, murs, avant-toits), maillages simple et fin |
| `AngVegetation001` | végétation en voxels |
| `DvFMV2020idx0001` | ventes DVF 2020, France |
| `OrthoBDORT2024v2`, `TerraLidHD2025v2`, `BuildLidHD2025v3`, `BuildLidHD2025f3`, `VoxelLidHD2025v1` | la commune d'Anglet entière (son contour : `experiences/scripts/world/boundaries/anglet-64024.geojson`) : `scripts/local/prep-commune.sh` (ci-dessous) ; celles du disque de 1 km restent construites par `prep-all.sh` tant que leurs entrées sont là |
| `TempeAROME0128v2`, `RainfAROME0128v2`, `CloudAROME0128v2`, `WindUAROME0128v2`, `WindVAROME0128v2`, `TownsAROME0128v2` | météo AROME : une collection par variable, une métrique par heure (`SolarAROME0128v2` est construit aussi, Explore ne le lit pas) |

## La commune entière : `prep-commune.sh`

```sh
cd ~/Development/github.com/indexus/_run/core
scripts/local/prep-commune.sh plan     # surface, téléchargement, disque estimés ; place libre
scripts/local/prep-commune.sh          # puis : scripts/local/explore.sh (importe les lots)
```

Pavés de 2 048 m, quarts de 1 024 m : un quart est téléchargé (COPC LiDAR HD,
par boîtes, reprise par boîte), préparé, réduit (végétation, sommes du sol,
toits), puis son brut et ses retours préparés sont effacés ; un pavé écrit ses
parts de végétation et ses bâtiments. Le disque reste borné (environ 12 Go au
pic pour Anglet, dont 6 Go de lots) ; le script vérifie la place libre avant
de commencer et avant chaque quart. Le travail est dans
`_run/.data/prep/commune/<scène>/` (un `done.json` par quart, une fiche par
pavé : une reprise ne refait rien de fini ; effaçable une fois les lots
importés). Détail : `tools/prep/commune/commune.py`.

## État : `_run/.data`

| Chemin | Contenu |
| --- | --- |
| `bin/` | `node`, `issuer`, `localdata`, `prep`, `prep-metal` et `prep.metal` |
| `node1/` | `node.json`, la base du nœud (`node.causal.db.records/`) et son identité |
| `issuer.ed25519`, `names` | clé de l'issuer, nom du nœud |
| `collections/`, `launch/` | entrées (voir plus haut) |
| `prep/lidar/<clé>/` | retours LiDAR préparés (un fichier IXRT par tuile de 32 m) |
| `prep/work/` | intermédiaires des constructeurs (sol, houppiers, scènes, météo) |
| `batches/<collection>/` | une batch IXP1 par collection |
| `imported/<collection>` | id de la batch installée (une batch inchangée n'est pas réimportée) |
| `networks.json` | le réseau déclaré à la gateway |
| `logs/` | `issuer.log`, `node1.log`, `prep.log`, `import.log`, `gateway.log`, `experiences.log` |
| `pids/` | un pid par service |

## Reconstruire une collection

L'id d'une batch est un hachage de ses entrées. Quand une entrée change,
`explore.sh` reconstruit la batch et la réinstalle avec `-replace` ; une
batch inchangée n'est ni reconstruite ni réimportée. Pour forcer une
collection (par exemple après un changement de constructeur) :

```sh
rm -rf _run/.data/batches/<collection>
scripts/local/prep-all.sh                   # reconstruit ce qui manque
_run/.data/bin/localdata -bulk _run/.data/batches/<collection> \
  -write http://127.0.0.1:21031 -issuer http://127.0.0.1:22030 -replace
```

Pour la météo, supprimer aussi `_run/.data/prep/work/weather` : son
constructeur se saute d'après la date de son descripteur.

Repartir de zéro : `explore.sh stop`, supprimer `_run/.data/node1` et
`_run/.data/imported`, relancer (les batches et les retours préparés sont
gardés).
