# Coût de l’inventaire des frontières pendant un poll de résumés

Une page de contributions doit découvrir les frontières que détient le vnode, trouver celles dont le parent appartient au demandeur, puis relire leurs valeurs. Si la liste de clés dépasse le budget de rétention par vnode, la page suivante recommence la découverte depuis le catalogue spatial. Le cache compact conserve davantage de frontières dans le budget existant de 1 Mio.

### Représentation et limite

Les petites listes gardent leur représentation native. Si elle dépasse le budget, le cache regroupe les clés triées par collection. Le nom de chaque collection est écrit une fois. Chaque location occupe, dans sa collection, autant d’octets que sa plus longue voisine plus un octet de longueur. Le curseur cherche directement le groupe et la première location strictement supérieure dans l’ordre lexical d’Indexus. Une page tardive n’a donc pas à décompresser les clés précédentes.

Soient B le budget, C la capacité allouée de la tranche de descripteurs, Nᵢ le nombre de clés du groupe i, Lᵢ sa longueur maximale de location et Kᵢ la longueur de son nom. Le texte contigu demande Σ(Kᵢ + Nᵢ(1+Lᵢ)) octets. Le code retient cette représentation si **128 + 64 C + capacité réellement allouée au texte ≤ B**. Le test emploie la capacité du tampon après l’arrondi de l’allocateur Go. Sur Go 1.26.0 darwin/arm64, avec une seule collection au nom de 16 octets et des locations de longueur 16, la limite compacte est **61 198 frontières**. Le nombre de groupes, leur profondeur et l’allocateur peuvent déplacer cette limite.

Si aucune représentation ne tient, le vnode répond avec la liste complète et la libère après utilisation. Cette borne porte sur le cache persistant des clés. Le catalogue spatial du store, les lecteurs encore actifs sur un ancien instantané, les réponses et le travail temporaire d’une reconstruction ont leurs propres coûts. Les clés sortant du cache compact sont détachées du tampon avant d’être conservées par une arête ou retournées dans une contribution.

### Mesure locale

Le microbenchmark construit 16 384 vraies frontières de profondeur 16, dans une collection. Chaque clé contient un record. Une vue à deux vnodes place toutes ces frontières chez un vnode et leurs parents chez l’autre. Chaque opération demande une page de taille un, vérifie la clé, la valeur un et la présence d’une page suivante. La première demande est effectuée après avoir amorcé la découverte. L’ancienne représentation ne pouvait pas conserver cette liste. Les mesures sont les médianes de trois répétitions par cas sur Apple M1 Pro.

| Page demandée | Avant sur e85a369 | Avec compactage | Octets alloués par appel avant → après | Allocations avant → après |
|---|---:|---:|---:|---:|
| Première page filtrée par parent | 23,76 ms | 3,43 ms | 13 771 781 → 3 319 884 | 151 116 → 49 188 |
| Après 16 382 clés déjà parcourues | 16,65 ms | 2,17 µs | 10 455 299 → 682 | 101 949 → 20 |

La première page filtre encore tous les parents candidats. La découverte initiale et la fusion d’un ensemble de frontières réellement nouveau gardent un coût proportionnel à la taille de l’inventaire. Pour isoler l’effet sur le CPU et la latence des écritures, il reste à reprofiler le détenteur chaud dans le banc AWS.

Depuis la racine du dépôt, la commande suivante reproduit le microbenchmark local.

```sh
go test ./internal/summary -run '^$' -bench '^BenchmarkLargeBoundaryContinuation$' -benchtime=400ms -count=3
```
