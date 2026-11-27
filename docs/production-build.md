# Build des binaires de production

Depuis la racine de `core` :

```sh
INDEXUS_VERSION=production-20261107 ./ops/deploy/scripts/build.sh
```

Les exécutables `node`, `issuer` et `provisioner` sont créés dans `bin/linux/`, ignoré par Git.
Un premier argument peut choisir un autre dossier de sortie. Les valeurs par
défaut sont Linux amd64 et CGO désactivé ; `GOOS`/`GOARCH` peuvent être fournis
explicitement pour une autre cible. L’architecture AWS actuelle est amd64.

Le script commun est appelé par les deux scripts de déploiement existants :
`core/ops/deploy/scripts/deploy.sh` et `dashboard/ops/demo/deploy.sh`.
Construire les exécutables ne modifie ni les données ni les services actifs.

## Options

- `-trimpath` retire les chemins locaux du build.
- `-ldflags "-s -w"` retire la table de symboles native et les informations DWARF.
- Le nœud et l’issuer embarquent leur version et leur commit ; `-dirty` signale un workspace modifié.
- L’exécutable reste autonome, sans compression auto-extractible ni changement
  de fonctionnalités. Les informations de stack Go restent disponibles ; le
  débogage natif nécessitant DWARF est plus limité.

`INDEXUS_VERSION` et `INDEXUS_COMMIT` acceptent lettres, chiffres, `.`, `_`, `+`
et `-`. Sans valeur explicite, le script prend les informations Git disponibles.

## Mesure du 7 novembre 2026

Même source et même version Go 1.26.0, cible Linux amd64 :

| Nœud | Octets | Mo décimaux |
| --- | ---: | ---: |
| Build avec débogage, comme le nœud déployé | 15 894 822 | 15,895 |
| Build de production sans symboles/DWARF | 11 051 170 | 11,051 |

Réduction : **30,5 %**. Une compression gzip destinée au transfert donne
4 450 822 octets ; le binaire installé reste l’exécutable non compressé.
L’issuer de production compilé par le même script mesure 6 492 322 octets.
Il s’agit de taille disque/transfert, pas d’une mesure de baisse équivalente du RSS.

Validation : build des deux exécutables, contrôle syntaxique des trois scripts,
contrôle ELF `stripped`, exécution de `node -version` sur l’instance AWS amd64.
Le test distant a utilisé un fichier temporaire supprimé ensuite. Le processus
servant les 564 837 records n’a pas été remplacé ni redémarré par cette mesure.
Les nouvelles options s’appliqueront au prochain déploiement via ces scripts.

## Simplification appliquée

Le nouveau nœud déployé mesure **8 773 794 octets**, avec les mêmes options de
build, soit **−20,6 %** par rapport aux 11 051 170 octets ci-dessus. L'issuer
mesure 6 299 810 octets. Les adaptations de configuration et les contrôles
sont décrits dans [production-configuration.md](production-configuration.md).
Le provisionneur est un outil distinct, non activé sur le serveur actuel.

## Bancs et profilage

Les bancs utilisent les mêmes options `-trimpath -ldflags "-s -w"` :
`ops/experiment/aws/run.sh` (node, issuer, provisioner, experiment, nœud
`quic/`) et `scripts/research/bulkbench.sh`. Mesure du 16 novembre 2026
(Go 1.26.0, Linux amd64, CGO désactivé), `-trimpath` seul puis avec `-s -w` :

| Binaire | `-trimpath` | `-trimpath -s -w` | Gain |
| --- | ---: | ---: | ---: |
| node | 13 476 452 | 9 375 906 | −30,4 % |
| issuer | 9 083 377 | 6 287 522 | −30,8 % |
| provisioner | 9 962 596 | 6 901 922 | −30,7 % |
| experiment | 11 349 931 | 7 893 154 | −30,5 % |
| node `quic/` | 16 149 951 | 11 198 626 | −30,7 % |

`-s -w` retire la table de symboles ELF et DWARF, pas la table `pclntab` du
runtime Go. Vérifié sur un build `-tags diagnostic` allégé : `/debug/pprof/`
(profil CPU, tas, goroutines), `/debug/stacks` et les traces de panique gardent
noms de fonctions, fichiers et lignes ; `go tool pprof -top` lit les profils sans
le binaire. Restent limités : `go tool nm`, `gdb`/`dlv` et `pprof -disasm`, qui
demandent un binaire non allégé du même commit. Avec `-trimpath`, les chemins
sont relatifs au module (`github.com/indexus/go-indexus-core/...`) : pour
`pprof -list`, passer `-source_path`/`-trim_path`.
