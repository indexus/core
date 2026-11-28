# Objectif produit d’Indexus

Permettre à un développeur de créer un cluster, d’y déposer une collection et d’en explorer progressivement les données depuis son application. Une même vue doit pouvoir alimenter une liste, un graphique, une carte, une scène 3D ou un jeu. Le choix du moteur de rendu appartient à l’application.

## Ce qui existe aujourd’hui

Le nœud Go sert des observations locales. Le SDK JavaScript peut découvrir les branches en parallèle, conserver sa progression, filtrer et agréger les documents observés, puis combiner recherche textuelle et vectorielle. Nearby restitue en O(X) les X résultats déjà prêts sur une frontière terminale triée. Découvrir une branche supplémentaire peut demander une lecture et un tri.

Le client peut vérifier à la demande l’empreinte d’un ancêtre, descendre si elle change et différer le rechargement de certaines branches. Une empreinte brute se compose sur des contributions disjointes. Le témoin de révision permet de détecter une écriture pendant le chargement. Chaque pair peut observer un état différent et une réponse peut rester inconnue. La position de lecture appartient au client. Le nœud conserve les données, ses agrégats et les informations de réplication. [Contrat des empreintes](../protocol/subtree-fingerprint.md) · [Guide SDK](https://github.com/indexus/sdk/blob/40d2c9c/docs/observed-cut-refresh.md)

## Expérience développeur visée

- **Cluster.** Une commande crée un cluster avec région, bornes d’autoscaling et plafond de dépense. L’endpoint reste stable lorsque des nœuds arrivent ou partent. Le service managé prend en charge les accès, les sauvegardes et le dimensionnement.
- **Collection.** Le développeur déclare l’identité des documents, les dimensions utiles au parcours et les agrégats disponibles avant chargement du détail. Une écriture renvoie une référence utilisable pour relire ou supprimer le document. Exemples de collections : bâtiments géolocalisés, observations météo horodatées, passages de manuels ou objets d’un monde 3D.
- **Vue.** L’application choisit une sélection et conserve sa vue entre deux interactions. L’API cible propose `summary()`, `next(n)`, `changes()` et `refresh({branches})`. Les opérations sur les données déjà chargées sont identifiables. La vue expose le budget consommé, les lectures et les branches incertaines. Ces noms décrivent la cible ergonomique.

## Du tableau au jeu vidéo

Pour un graphique météo, la vue peut servir des agrégats sur une journée puis charger les observations d’une heure sélectionnée. Pour un jeu, une collection pourrait décrire des objets par position, bornes spatiales et référence de ressource. En déplaçant la caméra, l’application donnerait priorité aux zones visibles, exploiterait d’abord une représentation grossière, puis demanderait davantage de détail dans son budget. Une édition du décor permettrait de tester le rafraîchissement d’une seule branche.

L’interface de rendu à prototyper livrerait les ajouts, modifications et retraits par lots. Le moteur graphique déciderait du niveau de détail visuel et de la mise à jour de ses buffers. Commencer par une scène Web 3D, puis reprendre la même collection dans un moteur de jeu natif. Mesurer le temps jusqu’au premier affichage utile, les octets transférés, les copies CPU/GPU, les lectures abandonnées après déplacement de caméra et le temps de trame au p95. La simulation multijoueur autoritaire relève d’un autre contrat que la lecture éventuellement convergente du décor.

## Modifications en direct et propagation des zones modifiées

Le SDK sait déjà vérifier une empreinte à la demande. L’objectif supplémentaire est de prévenir rapidement une application ouverte lorsqu’une écriture touche une zone qu’elle observe. Une vue pourrait s’abonner aux préfixes utiles et recevoir un signal léger indiquant la collection et la zone susceptible d’avoir changé. Le mode à la demande reste disponible. La diffusion des signaux peut être mutualisée entre clients, tandis que chaque vue conserve localement son parcours, ses priorités et sa dernière observation validée.

Dans un jeu multijoueur, si un joueur construit un mur, les autres joueurs qui regardent cette partie du monde devraient voir le changement sans recharger la scène. Si un objet passe d’une zone à une autre, la modification concerne la zone de départ **et** celle d’arrivée. Sur une carte d’interventions partagée, le même mécanisme permettrait à un opérateur de voir un changement de statut effectué par son collègue.

Le signal invite à vérifier la zone. Le client regroupe les notifications en rafale, compare les empreintes et recharge en priorité les branches visibles. Une suppression doit aussi pouvoir retirer un objet de l’affichage. Des signaux dupliqués, retardés ou reçus dans le désordre doivent laisser la vue converger. Après une déconnexion, le client revérifie les préfixes suivis pour retrouver aussi les changements dont il a manqué la notification. Un transfert entre pairs peut rendre une observation temporairement incertaine.

Qualifier ce mode avec deux clients qui modifient et observent la même scène. Mesurer le délai entre l’écriture acquittée et le premier affichage correct (p50/p95/p99), les octets et lectures par changement visible et le délai de rattrapage après reconnexion. Couvrir la création, le déplacement entre zones et la suppression, y compris pendant un transfert de propriétaire. Pour les jeux exigeant un arbitrage immédiat des collisions ou des actions concurrentes, préciser séparément les garanties du serveur de simulation.

## Trajectoire technique proposée

1. Extraire en Rust les règles communes du protocole : adressage, formats, agrégats et empreintes. Vérifier les mêmes vecteurs de référence en Go, Rust et JavaScript.
2. Construire le moteur de vue en Rust. Le compiler en WASM pour le navigateur et en bibliothèque native pour les autres SDK. Le moteur décide des branches à lire ; le SDK hôte exécute les lectures parallèles, gère les accès et l’annulation. Échanger des blocs et des pages entières. Comparer démarrage, copies et pagination au SDK actuel.
3. Livrer d’abord TypeScript, puis Python pour valider la portabilité, et Swift/Kotlin pour le mobile. Les adaptateurs React, graphiques et moteurs de jeu gèrent le cycle de vie d’une vue et les conventions de leur plateforme.
4. Porter le nœud en Rust en gardant Go comme référence. Qualifier lectures, écritures, suppressions et redémarrage, puis un transfert de zone entre nœuds Go et Rust sous écritures concurrentes. Le client doit pouvoir poursuivre son parcours et retrouver les données attendues après convergence.

## Critères d’adoption et de mesure

Viser moins de dix minutes entre la création d’un cluster et la première carte sur un GeoJSON du développeur. Vérifier que la même application fonctionne lorsque le cluster passe d’un à plusieurs nœuds. Comparer à qualité équivalente les premières requêtes, les sessions réutilisant une sélection et les rafraîchissements sous mutations. Les résultats locaux sur SIFT et les pilotes sur deux pairs constituent les premiers repères. La comparaison distante à 10 millions de documents reste à exécuter. [Campagne, commandes et références turbopuffer](../benchmarks/search-campaign.md)
