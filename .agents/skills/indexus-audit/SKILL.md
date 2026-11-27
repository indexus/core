---
name: indexus-audit
description: Mesure l'état des couches d'Indexus — lignes par couche face aux budgets, imports interdits entre couches, fichiers et lignes trop longs, travail non commité. À lancer avant et après un nettoyage, quand on demande un état des lieux, un audit, « où en est le code », ou pour vérifier qu'un changement respecte la charte.
---

# Audit des couches

```bash
bash core/.agents/skills/indexus-audit/scripts/audit.sh            # depuis la racine indexus
bash core/.agents/skills/indexus-audit/scripts/audit.sh /chemin/vers/indexus
```

La racine est le dossier qui contient `core`, `sdk`, `dashboard` et
`experiences` ; sans argument, le script la cherche au-dessus de lui.

Le script ne modifie rien. Il n'a besoin que de `bash`, `rg`, `awk` et `git`.

## Ce qu'il sort

1. **Budgets** : lignes hors tests par couche, cible, écart
   (`indexus-layers/references/budgets.tsv`).
2. **Imports interdits** : chaque ligne est une violation de la charte, avec
   `fichier:ligne`.
3. **Poids** : fichiers de plus de 600 lignes, nombre de lignes de plus de
   160 caractères.
4. **Git** : branche et nombre de fichiers non commités par dépôt.

## Comment s'en servir

- Avant un nettoyage : garder la sortie. Après : relancer et comparer. Le compte
  rendu donne les deux.
- L'état de départ de la charte (13 novembre 2026, avant la vague 3) est dans
  `references/baseline-2026-11-13.txt` : un repère historique, pas l'état actuel.
- Une violation se corrige en déplaçant le code, pas en ajoutant une exception
  au script. Une exception durable se note dans `indexus-layers` avec sa raison.
- Une nouvelle règle de couche = une ligne de plus dans la section « règles »
  du script.

## Ce que le script ne voit pas

Le code sans appelant, les doublons de concept et les allocations par item. Pour
ça : `rg` symbole par symbole (méthode dans `indexus-simplify`) et un profil
mémoire (règle 10 de `indexus-gpu-path`).
