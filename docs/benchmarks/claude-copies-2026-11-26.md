# Intégration des copies de staging — 26 novembre 2026

Les trois commits de `claude/next` sont fusionnés en avance rapide sur main
et poussés jusqu’à **`36e2b31`**. La suite Go et les tests de concurrence passent,
mais **la qualification multi-processus échoue** sur un total exact indisponible.
Cette version n’est pas qualifiée pour une nouvelle campagne AWS.

## Changements relus

- `6c64964` : les emplacements de l’agent partagent la limite de tas de la
  machine, avec conservation d’un `GOMEMLIMIT` explicite. Cela corrige un budget,
  pas une consommation RAM mesurée pendant le run AWS précédent.
- `27f8677` : `items_counted` et `items_age_ms` distinguent un compteur non
  encore calculé d’un vrai zéro. Pas de comptage supplémentaire.
- `36e2b31` : un index en mémoire du nombre de copies de reçus en staging,
  reconstruit à l’ouverture et maintenu après commit sous `writeMu`. Le calcul
  de réservation peut éviter les sessions sans copie concernée, tout en
  conservant le parcours de toutes les sessions quand la preuve locale manque.
  Aucun format durable ne change. La mémoire supplémentaire est proportionnelle
  aux requêtes en staging ; son pic n’est pas mesuré dans cette qualification.

Les mutations d’index sont appliquées après le commit effectif. Les contrôles
de stockage des tests comparent l’index avec les lignes réelles. Le partage de
mémoire reste une limite souple du runtime Go et ne réserve ni CPU ni RAM
physique pour une copie.

## Vérifications effectuées ici

- `go test -count=1 ./...` : vert, sans exclusion ajoutée.
- `go test -race -count=1 ./internal/storage ./internal/autoscale
  ./cmd/experiment ./internal/adapter/http/monitoring ./internal/mesh` : vert.
  Mesh complet : 286,846 s sur cette machine.
- Binaires Linux arm64 reconstruits depuis le checkout propre à `36e2b31`.
- Un passage `placement.py --trigger demand`, même image, seed et seuils que
  le [passage antérieur](settled-demand-2026-11-26.md). Aucune relance après échec.

## Échec multi-processus conservé

Le générateur demande et acquitte 17 999 écritures. L’audit relit **les 17 999
clés**, sans manque ni erreur, et n’observe aucune double propriété. Il attend
21 sondages, 5 075 ms, puis observe zéro mouvement avant l’inventaire.

Sur les 17 collections, le total exact de `Placeh0000000000` répond :

```text
status 503: cluster transaction is not ready: inventory changed during paginated read
```

L’audit conserve `total_errors`, `items_match_acked=false`, `pass=false`.
Il ne transforme pas ce refus en zéro et ne réessaie pas le total pour obtenir
un succès. La somme consultative `items_held=24049` n’est pas une cardinalité exacte.

**Lu dans le code :** `inventory_page.go` utilise `tx.ID()` comme watermark et
invalide une page suivante si cette version change. Ce garde-fou couvre une
version transactionnelle globale, pas uniquement les données de la collection
interrogée. Une absence de transfert observée n’équivaut pas à l’absence de
commit pendant toutes les pages. Il reste à identifier le commit invalidant
dans ce passage et à définir une vue stable ou une invalidation mieux ciblée,
sans retourner de total incomplet.

Le même message de 503 existait déjà sur une version antérieure. Un passage
vert avant et un rouge après, avec des identités de nœuds différentes, ne
permettent pas d’attribuer sa cause au nouvel index de staging. Les assertions
existantes restent intactes ; les conteneurs et le réseau de cet essai sont
supprimés. Aucun déploiement AWS de `36e2b31` n’a suivi.

[Audit](claude-copies-20261126/audit.json),
[replay rouge](claude-copies-20261126/live.html),
[tests et empreintes](claude-copies-20261126/manifest.json).
