# Qualification de 9ec4688 — 26 novembre 2026

- **Qualification arrêtée avant AWS : le déploiement n'installerait pas le hook du nouveau placement.**
- Les onze commits ont été poussés sur `main`, après vérification exacte de l'arbre et du serveur distant.
- Formatage, compilation et vet réussissent ; la suite normale a été interrompue au défaut de câblage, sans être déclarée verte.
- Labo 64 nœuds, cluster local réel et AWS non exécutés ; aucune ressource AWS créée pour cette qualification.
- Aucun correctif de production, aucune assertion affaiblie, aucune modification de `claude/next`.

## Version et méthode

Source examinée : `9ec4688c85e3daeae8bbb367b6e2401f414c0cd8`.
Arbre attendu et constaté : `29fb5548f554d8d270fdd0a8a2dc033acee62e07`.
Avant le push, `origin/main` désignait bien `a937b18`, sans divergence.
Un worktree détaché de qualification protège la source et les travaux parallèles.
Les fichiers non suivis du checkout principal ont été conservés.

La consigne du relais demandait explicitement de ne rien développer pendant
cette passe et de s'arrêter au premier défaut en conservant sa reproduction.
L'arrêt porte sur le câblage du harnais AWS, pas sur une assertion d'autorité
ou un échec constaté de la suite de tests.

Les journaux et reproductions sont dans le workspace :
`outputs/qualification-9ec4688-20261126/`. Les observations suivantes distinguent
mesures locales, lecture de code et résultats rapportés par Claude.

## Point sur les onze commits

| Groupe | Changements lus dans le code | Ce que cela vise |
| --- | --- | --- |
| `21d7674` | Énumération des sessions de staging sans charger leurs valeurs ; première page de huit lignes | Retirer un travail inutile de contrôle de capacité, fréquent chez les receveurs |
| `b0f2a87` | Treize fonctions de transfert, inventaire, reprise et collecte découpées en étapes nommées | Réduire l'emboîtement et expliciter la portée des verrous ; équivalence fonctionnelle encore à qualifier |
| `2ff28f8` | Arrêt du rétrécissement des lots lorsqu'il n'apporte pas 25 % de gain ; grain adaptatif de 256 Kio à 16 Mio, cible de clôture régionale 100 ms | Éviter de multiplier les commits sans raccourcir le gel ; conserver la borne de 512 zones |
| Six commits `5ec1a11` à `abd12f9` | Endpoint depuis le contrôle engagé ; page d'export depuis une seule version ; curseur de staging ; suppression de lectures inutiles à l'installation ; réutilisation des scellés du fence ; une conversion de record pour le journal | Enlever des lectures, encodages, empreintes et reconstructions répétées |
| `bc78a25` | Audit des travaux en double et distinction entre corrections faites et propositions | Garder les coûts restant à traiter visibles |
| `9ec4688` | Placement par ressources physiques, `/facts`, mémoire des placements, métriques de décision ; séparation `Open`/`Join`, monitoring avant l'attente du join | Éviter deux receveurs chauds sur le même hôte et rendre le vnode observable plus tôt |

`FenceZones` retourne maintenant quatre valeurs, dont les scellés déjà calculés.
La compilation du module principal confirme que ses appelants compilés sont
adaptés. Cela ne vaut pas validation des reprises ni du cluster H3 complet.
Les jobs restent lancés après la première tentative de join ; seul le monitoring
est déplacé avant cette attente. Le signal de demande fondé sur les complétions
et `Queue: 0` reste un point ouvert du document de placement.

**L'aplatissement n'est pas une réduction globale des lignes.** Le diff des
fichiers Go de production, hors `_test.go`, entre `a937b18` et `9ec4688` est
**+1 252 lignes nettes**. Mesh et storage représentent ensemble +611 lignes ;
la nouvelle politique, son intégration et les autres changements représentent
le reste. La simplification porte sur les étapes et le travail évité ; elle
ne doit pas être présentée comme une baisse du volume total de code.
Le décompte par chemin est conservé dans `production-line-delta.json`.

### Mesures rapportées, non reproduites ici

Claude rapporte un labo local à deux cœurs, huit nœuds, 60 000 zones et
400 écritures/s : clôture des joins 114 s avant, 63 s après sur deux passages ;
p99 191 ms avant, 185 et 148 ms après, audits propres. Ce ne sont pas des
mesures AWS et le placement de machines n'a pas été exercé par ces passages.
Aucun tableau avant/après à 64 nœuds n'est produit dans cette qualification.

## Qualification effectivement réalisée

| Étape | Résultat de cette passe |
| --- | --- |
| Empreinte de source et contrôle du distant | Confirmés, puis onze commits poussés |
| `gofmt -l .` | Réussi, aucun fichier signalé |
| `go build ./...` | Réussi |
| `go vet ./...` | Réussi |
| `go test -p 1 ./...` | Commencé, puis interrompu lors de la confirmation indépendante du défaut AWS ; ni PASS global ni échec d'assertion déclaré |
| Suites `-race` demandées et `test_spawn.py` | Non exécutés dans cette passe |
| Labo dynamique 64 nœuds, deux répétitions et fautes | Non exécuté |
| Cluster local réel, placement et invariants pendant joins | Non exécuté ; précondition disque insuffisante également constatée |
| AWS | Préflight en lecture seule ; aucun lancement |

La vérification exhaustive d'usage était laissée dans son mode par défaut.
Les succès de tests annoncés par Claude ne sont pas réattribués à cette passe.
La sérialisation des paquets par `-p 1` visait à éviter les fausses expirations
observées précédemment quand plusieurs suites lourdes partageaient le portable.

## Défaut bloquant 1 — facts.sh n'est pas installé sur les hôtes

**Lecture de code et reproduction statique déterministe.**

- `ops/experiment/aws/run.sh:217` copie tous les scripts élastiques, y compris
  `facts.sh`, dans le paquet envoyé à S3.
- `ops/experiment/aws/host.sh.tftpl:34` télécharge une liste explicite :
  `node issuer provisioner experiment elastic/agents.sh elastic/spawn.sh
  elastic/count.sh elastic/terminate.sh ca/ca.crt ca/ca.key`.
- **`elastic/facts.sh` manque dans cette liste.**
- Le provisioner est lancé avec `-localDir /opt/indexus/elastic`.
  `machineFacts`, `internal/adapter/provisioner/placement.go:49`, cherche
  automatiquement `facts.sh` dans ce dossier et retourne nil en son absence.
- Sans faits, `decidePlacement` choisit explicitement `PlaceSlots`, avec la
  raison `no resource facts: slot-count rule of the hook`.

Un cluster AWS frais ne testerait donc pas le nouveau placement annoncé.
Aucun flag `-facts` n'est requis : la découverte du hook est automatique.
Le défaut est le téléchargement omis, pas un flag manquant.

Reproduction sans AWS :

```sh
sed -n '202,220p' ops/experiment/aws/run.sh
sed -n '34p' ops/experiment/aws/host.sh.tftpl
sed -n '46,53p' internal/adapter/provisioner/placement.go
sed -n '86,94p' internal/adapter/provisioner/placement.go
```

Le fichier `deployment-wiring-reproduction.json` conserve les résultats :
`facts_uploaded=true`, `facts_downloaded=false`. Ce contrôle statique n'est
pas présenté comme une observation d'un cluster en fonctionnement.

## Défaut de comparaison 2 — le loader fournit aussi l'agent de machine

La commande comparative préparée garde le loader historique `2362b35`, comme
lors des essais précédents. Or `run.sh:206–209` compile **experiment** depuis
`LOADER_ROOT` et les hôtes installent ce même binaire pour leurs agents.
Le rôle de générateur et celui d'agent ne sont pas séparés par ce harnais.

Avec cette configuration, les agents seraient issus de `2362b35`, où le
handler `/facts` n'existe pas, alors qu'il existe à `9ec4688`. Le manifeste
et la reproduction statique enregistrent cette différence.
Avec le loader courant par défaut, l'agent serait courant : ce deuxième défaut
est donc conditionnel au choix du loader commun historique. Le premier défaut
reste présent dans les deux cas.

Il faut distinguer la version du générateur commun et celle de l'agent associé
à chaque version testée pour que la comparaison ait le sens annoncé.
Simplement changer les deux agents de la référence historique masquerait une
partie des différences de cycle de vie que l'on cherche à mesurer.

## Exigence non câblée — profil CPU des receveurs en H3

Le build standard H3 remplace le nœud par `quic/cmd/node`. Ce point d'entrée
appelle `nodeapp.Main(version, commit, nil)`. Le branchement diagnostic du
module principal (`cmd/node/diagnostic.go`) n'est pas importé par ce programme.
Ajouter seulement `-tags diagnostic` au build H3 ne branche donc pas pprof.

Les stacks textuelles restent accessibles par le dispositif existant ; cela
ne fournit pas le profil CPU demandé des receveurs. Il manque un dispositif
explicite de profilage pour l'essai H3, ou un profil système externe dont les
versions, commandes et coûts seraient tracés. Aucune instrumentation nouvelle
n'a été développée dans cette passe.

## Préconditions et préflight, non autorisation de lancement

Le disque réel du portable présente environ **5,9 % d'espace libre**, alors
que la politique exige au moins 35 % pour une cible éligible. Tous les agents
locaux partageraient ce disque. Les trois décisions normales local/existing/new
ne peuvent pas être présentées comme qualifiées avec des faits réels dans cet
environnement. Ni les faits ni le seuil n'ont été modifiés pour faire passer
ce contrôle ; aucune image à capacité virtuelle n'a masqué le manque de place.

Préflight AWS en lecture seule : zéro expérience active, quota standard
512 vCPU, deux vCPU d'autres usages, prix interrogés 0,398 USD/h par hôte
`i4i.xlarge` et 0,808 USD/h pour le loader `c6i.4xlarge`.
L'enveloppe **proposée, non confirmée pour cette passe**, de 11 USD avec TTL
45 minutes et plafond 32 hôtes permettrait 64 vnodes, deux par machine.
Une enveloppe de dimensionnement n'est pas un coupe-circuit de facturation.
Aucune ressource n'a été créée et aucun coût de campagne n'est revendiqué.

## État confirmé, infirmé et restant à vérifier

- Confirmé par cette passe : source exacte et push, format/build/vet,
  câblages manquants ci-dessus, décompte de lignes et préflight en lecture seule.
- Infirmé : la commande AWS comparative préparée déploierait automatiquement le
  nouveau placement et permettrait déjà de profiler le CPU des receveurs H3.
- Non vérifié ici : équivalence des treize refactorings, écrivain unique et ACK
  sous fautes sur cet arbre, p99 à 64 nœuds, gros grains en octets, placement réel,
  monitoring pendant un join et stabilité jusqu'à 16k/s.

La reprise demande d'abord une correction séparée du câblage du harnais, puis
une version figée et une qualification reprise dans l'ordre. Une confirmation
explicite du budget reste requise avant un lancement AWS. Aucun correctif n'a
été appliqué et aucune assertion n'a été affaiblie pendant cette passe.
