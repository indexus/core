# Lectures documentaires pilotées par le client

Avant de choisir une primitive, écrire ce qu’un client peut déduire d’une
réponse vide. Une lecture directe autorisée peut établir qu’une adresse ne
porte aucun document. Un résumé de cache manquant ou une observation locale
d’enfants pendant un transfert laisse la branche à explorer. Un document
peut aussi résider directement sur une adresse intermédiaire.

## Du stockage au SDK

- Pour un résultat autoritaire à une adresse, lire les droits installés et
  les lignes dans le même instantané local. Le code de `/records/direct`
  illustre cette règle dans `internal/storage/direct_records.go`.
- Pour une observation partielle, indiquer sa portée sur le fil et la
  conserver dans le résultat du SDK. Une opération sur plusieurs réponses
  reste partielle tant que leur couverture n’est pas établie. Pendant un
  transfert, un donneur peut conserver des lignes que le receveur a supprimées.
- Une petite réponse peut cacher un grand scan. Examiner le parcours de la
  memtable et des segments, y compris tombstones et versions. Faire varier le
  nombre de descendants à résultat identique. Préciser la dépendance au nombre
  de segments, aux pages ou aux clés visitées selon les compteurs disponibles.
- Pour le chemin de lecture local, le SDK choisit le pair et l’adresse. Les
  refus et indisponibilités remontent au client, qui conserve sa progression.
  Un appel à `QueryPrefix` ou une lecture fraîche via le maillage change la
  portée de l’opération. La politique des routes historiques figure dans
  `docs/protocol/query.md`.

## Vérifier le contrat réellement livré

Lorsque la modification traverse la frontière Go/JS, le banc
`internal/app/node` peut démarrer deux serveurs HTTP et exécuter le SDK
indiqué par `INDEXUS_TEST_SDK` (chemin absolu vers le checkout SDK).
`TestDirectSDKFacetsAndOrderTwoPeers` illustre ce montage. Compter séparément les
appels de données et les sondes de cache. Pour une évolution de la visibilité
pendant transfert, reprendre le cas du préfixe partagé dans
`indexus/child_discovery_handoff_test.go`. Un test sur des adresses connues
et stables qualifie la composition des résultats dans ce périmètre.
