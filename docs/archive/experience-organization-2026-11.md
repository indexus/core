# Organisation commune des expériences

## Un seul chemin réseau

Les quatre expériences se connectent par le SDK. `GatewaySession` centralise la
configuration du réseau actif, les pairs, le token renouvelable et la création
idempotente du `Network`. Il ne crée aucun service intermédiaire. Le proxy P2P
existant transporte le protocole ; les lectures de données restent celles du SDK.

L’ancien adaptateur Python était un serveur sur le port 3867 qui convertissait
les URLs de fichiers en lectures `/records`. Son code, ses tests, son processus
et sa configuration ont été retirés. Le Python restant prépare/importe les
sources ou lance les services locaux ; il ne sert plus les blocs aux expériences.

## Arborescence

```text
weather/src/
  data/
    network.js       # GatewaySession commun au navigateur et aux workers
    assets.js        # ContentAssets et chemins canoniques
    collections.js   # identités des collections de l'expérience
  experiences/
    weather/         # main.js, worker.js, style.css et logique météo
    point-cloud/     # main.js, worker.js, style.css et logique LiDAR
weather/test/
  data/
  weather/
  point-cloud/
  support/           # modèles de référence scientifiques
  fixtures/

dashboard/src/experiences/
  data/network.js    # même GatewaySession du SDK
  drawing/          # main.jsx, style.css, modèle et codec
  realestate/       # main.jsx, style.css
dashboard/test/drawing/
```

Les scripts de préparation restent sous `scripts/`, les modèles préparatoires
sous `scripts/lib/`, les rapports sous `.local/`, et les notes sous `docs/`.
Chaque expérience garde ses paramètres de concurrence/cache selon sa charge ;
le protocole et la connexion sont communs, pas les responsabilités métier.

## Simplifications

- Plus de port de bootstrap codé en dur pour les versions publiques dessin/DVF.
- Plus de barre de navigation abandonnée ni de point d’entrée 3D vide.
- Entrées `main` / `worker` / `style` identiques pour météo et 3D.
- Les contenus du dessin restent des records SDK simples, sans conversion en
  anciennes méthodes `id()/hash()/metrics()` ni repli vers un autre lecteur.
- Le dessin écrit ses chunks via un seul chemin et garde sa protection de
  publication/journal ; la lecture des cellules existantes reste préservée.
- Le codec déclare sa dépendance binaire explicitement, au lieu de dépendre d’un
  `Buffer` global ajouté par le point d’entrée.
- La commande de test du dashboard inclut maintenant ses tests du dessin.

Les données, les collections, les fichiers source et les sauvegardes sont
conservés. Les fonds OSM/MapTiler et le masque France restent des références
cartographiques externes/statiques. Aucun déploiement de production.

## Vérification après réorganisation

- SDK : 209 tests ; météo et 3D : 102 tests ; dashboard et dessin : 66 tests.
- Builds SDK, météo, 3D et dashboard en mode `experiences` réussis.
- Dans Chrome : météo renseignée, scène Lyon « Prêt », carte DVF chargée,
  dessin « Synchronisé avec Indexus ».
- Lecture SDK directe vérifiée pour les manifestes des cinq scènes, la météo,
  les 69 records du dessin et les 546 278 ventes DVF.
- Régression navigateur corrigée : le `fetch` natif est lié au contexte global,
  aussi bien dans la fenêtre que dans les workers ; test dédié ajouté.

Les builds signalent encore de gros chunks JavaScript (> 500 kB). Cette
réorganisation ne constitue pas une mesure de performance ni une réduction
quantifiée de la mémoire.
