# Apprentissage continu par sommes de zones : expérience du 20 novembre 2026

Question : peut-on apprendre en continu, et oublier exactement, avec la seule
primitive d'Indexus (des sommes repliées sur un arbre de zones) ?
Le modèle est une fonction des seules statistiques suffisantes
n, Σy, Σx·yᵀ, Σx·xᵀ ; ajouter une donnée est une addition, la supprimer une
soustraction.

## Protocole

- Données publiques : HuffPost News Category (209 527 articles, 42 catégories,
  2012 → 2022, dérive réelle d'une année à l'autre) et AG News (127 600 textes,
  4 classes).
- Deux encodeurs gelés : `all-MiniLM-L6-v2` et `bge-small-en-v1.5`, 384 dim.,
  normalisés, stockés en float16.
- Têtes calculées par sommes : ridge (moindres carrés sur one-hot), LDA à
  covariance partagée rétrécie, NCM ; zones de l'espace (variante 1 : projection
  aléatoire, quantiles gaussiens, bissection) avec un prior vers la zone
  parente ; zones par année choisies à la lecture.
- Références entraînées : régression logistique (lbfgs), SGD en ligne,
  ré-entraînement complet chaque année, modèle de la première année seule.
- Vérification dans le vrai moteur `zonelog` (50 000 exemples, 560 colonnes par
  record) : plusieurs ordres d'écriture, suppressions, ré-écritures.
- Recalcul indépendant des chiffres clés avec scikit-learn seul (`verify.py`).

## Résultats

| Affirmation | Verdict | Chiffres |
|---|---|---|
| Même modèle quel que soit l'ordre ou le découpage | confirmé | numpy : 0 prédiction changée, poids à 9e-15 ; `zonelog` : totaux identiques au bit près entre deux ordres d'écriture |
| Oubli exact | confirmé | suppression de 10 %, d'une classe, de 2012 : prédictions identiques au recalcul ; `zonelog` : après suppression = store construit sans ces données, au bit près ; attaque d'appartenance AUC 0,74 → 0,50 |
| Pas de dérive numérique | confirmé en float64 Neumaier, réfuté en float32 | 4 000 cycles insert/suppression : Neumaier stable à 1e-16 ; float64 simple 4e-16 et croissant ; float32 8e-6 et des prédictions changent |
| Aussi bon qu'une sonde entraînée | confirmé (facile), réfuté (difficile) | AG News : −0,4 à −0,8 pt ; HuffPost 42 classes : −5,2 à −5,3 pts (−2,1 pts avec 4 096 features de Fourier aléatoires, toujours par sommes) |
| Les zones de l'espace aident | faible | +1 à +2 pts |
| Classes qui arrivent une à une | confirmé, net | sommes 89–90 % (AG) et 55–58 % (HuffPost), égal à l'apprentissage joint ; SGD 25 % et 15 % |
| Dérive temporelle | mitigé | oubli (BWT) −0,6 pt contre −14 pts pour SGD ; mais le modèle cumulé s'adapte mal (48,6 % contre 57,1 % pour SGD en prévision de l'année suivante) ; fenêtre de 2 ans : 56,4 % |
| Zones par année, choisies à la lecture | intéressant | 66–67 % sur toutes les années, BWT ≈ 0, 1,5 s pour tout le flux ; ré-entraînement cumulé 59,7 % ; une sonde entraînée par année fait mieux : 69–71 % |

Coûts mesurés : intégrer les 11 années coûte 0,04 à 2 s par sommes contre 312 à
482 s de ré-entraînement (×400 à ×10 000) ; un recalcul complet est une
résolution 384×384 de 5 ms ; `zonelog` écrit 94 000 records/s et supprime
11 000 records/s sur le Mac.

## Contre-exemples et limites

- Ne jamais accumuler en float32.
- Les suppressions sont environ 8 fois plus lentes que les écritures dans `zonelog`.
- Écart de 3 à 5 points face à une sonde entraînée sur une tâche à 42 classes.
- Pas testé : la convergence entre plusieurs nœuds (un seul store ici), ni
  l'échelle au-delà de 200 000 exemples.

## Travaux proches (à citer)

La partie apprentissage est connue :
- ACIL (Zhuang et al., NeurIPS 2022) et DS-AL (2024) : équivalence entre
  apprentissage incrémental et joint, « mémorisation absolue ».
- AFL (He et al., CVPR 2025) : invariance au partitionnement entre clients.
- ACU (Tang et al., 2025) : oubli continu exact par moindres carrés.
- Deep SLDA (Hayes et Kanan, CVPRW 2020) : LDA en flux, ×100 en vitesse.
- RanPAC (McDonnell et al., NeurIPS 2023) : projection aléatoire non linéaire
  et matrice de Gram, ce qui recoupe nos features de Fourier aléatoires.
- À vérifier : apprentissage dans la base par agrégats (Olteanu, LMFAO) et
  regression cubes (Chen, Han et al., 2006).

Ce qui semble propre à Indexus (non trouvé dans une recherche rapide) : ces
statistiques comme primitive d'une base distribuée, repliées sur un arbre de
zones, avec des totaux déterministes au bit près, un modèle choisi par zone au
moment de la lecture, l'oubli au niveau de l'enregistrement et une description
de collection hachée qui rend le tout vérifiable.

## Contenu et reproduction

- `embed.py`, `aws/` : calcul des embeddings sur une instance jetable
  (VPC isolé, sortie internet seule, résultats par URL S3 présignées, TTL).
  Coût de la campagne : ≈ 0,80 $, tout détruit et vérifié.
- `analysis/` : `cl.py` (statistiques et modèles), `exp_exact.py`, `exp_cl.py`,
  `exp_gap.py`, `exp_time.py`, `exp_time_lr.py`, `verify.py`, `zc_prep.py`.
- `zonecheck/` : passage des statistiques dans le vrai `zonelog`
  (`build.sh` compile contre une copie de l'arbre).
- `results/` : JSON et journaux bruts de chaque run.
- Les embeddings (520 Mo) ne sont pas versionnés : `_work/datasets/cl/`
  sur le Mac, ou à régénérer avec `aws/launch.sh`.

```sh
python analysis/exp_exact.py data/huffpost__all-MiniLM-L6-v2.npz 1000000000 400
python analysis/exp_cl.py    data/huffpost__all-MiniLM-L6-v2.npz all
python analysis/exp_time.py  data/huffpost__all-MiniLM-L6-v2.npz
```
