---
name: indexus-simplify
description: Méthode de nettoyage et de refactoring d'Indexus sans changer le comportement — supprimer le code non branché, fusionner les doublons, déplacer un fichier vers sa couche, raccourcir une fonction. À utiliser quand on demande de nettoyer, simplifier, refactorer, fusionner, réorganiser ou réduire du code dans core, sdk, dashboard ou experiences.
---

# Simplifier Indexus

But : moins de concepts, pas seulement moins de lignes. Lire d'abord
`indexus-layers` : on simplifie **vers** la charte, de haut en bas.

## Avant de toucher

1. `git status --short` dans le dépôt concerné. S'il y a du travail non commité
   qui n'est pas le vôtre, **s'arrêter et demander** : ne jamais mélanger un
   nettoyage avec des changements en cours.
2. Lancer les tests et noter le résultat de départ (un test déjà rouge reste
   hors périmètre, on le signale) :
   - `core` : `go build ./... && go vet ./... && go test ./...` (long : `INDEXUS_LONG=1`)
   - `sdk` : `npm test`
   - `dashboard` : `node --test lib/*.test.mjs src/lib/indexus/*.test.mjs test/*.test.mjs`
   - `experiences` : `npm test`
3. Lancer `indexus-audit` et garder la sortie : c'est le « avant ».

## Ordre de travail (du moins risqué au plus risqué)

1. **Supprimer** ce qui n'a aucun appelant en production. Preuve exigée :
   `rg -n '\bNom\b' -g '!*_test.go' -g '!*.test.*'` ne renvoie que la définition.
   Si seuls des tests l'appellent : déplacer les helpers qui assemblent un vrai
   chemin de production en `_test`, ou supprimer le mécanisme avec ses tests
   orphelins. Ne pas déplacer un ancien algorithme dans les tests pour continuer
   à tester uniquement cet algorithme débranché.
2. **Fusionner** les doublons : garder la version de la couche la plus haute,
   rediriger les appelants, supprimer l'autre. Jamais de troisième version.
3. **Déplacer** vers la bonne couche, sans modifier le contenu dans le même commit.
4. **Raccourcir** : fonction > 80 lignes, struct > 15 champs, signature > 5
   paramètres ou avec plusieurs booléens, ligne > 100 caractères que le
   formateur ne sait pas casser.
5. **Changer un contrat** (format de fil, API publique, schéma disque) : ce n'est
   plus du nettoyage. Voir `indexus-gpu-path`, et le faire dans un travail séparé.

## Règles

- **Un changement, un commit, tests verts entre chaque.** Message :
  `refactor(<couche>): <verbe> <quoi>` avec le delta de lignes.
- **Comprendre avant de supprimer.** Un code étrange a peut-être une raison :
  `git log -S'Nom' --oneline`, et la ligne correspondante de
  `core/docs/protocol/verification.md`. Si un test nommé dans ce document
  disparaît, mettre à jour la ligne et son statut dans le même commit.
- **Ne pas affaiblir une garantie.** Une ligne `PROVED` ne redevient pas
  `PARTIAL` au nom de la simplicité.
- **Pas d'abstraction nouvelle** pour fusionner deux usages. On en introduit une
  au troisième, pas avant.
- **Le style est celui du formateur** (voir « Style »). Pas de fonctions tassées
  sur une ligne, une ligne vide entre déclarations, commentaires qui disent le
  *pourquoi* et non l'historique des incidents.
- **Le SDK se consomme par son nom public.** Pas d'import relatif d'une app vers
  `sdk/src/...`, pas d'auto-import `"js-indexus-sdk"` à l'intérieur du SDK.
- **`dist/` et les artefacts ne se versionnent pas.**

## Style

Une seule syntaxe dans les quatre dépôts. Le formateur décide, pas la personne.

- **Fichiers communs**, identiques partout : `.editorconfig` (utf-8, lf, newline
  finale, espaces, indentation 2 ; Go en tabulations ; Python 4 ; largeur 100).
  Dans `sdk`, `dashboard`, `experiences` : `.prettierrc.json`
  (`{"printWidth": 100}`) et `.prettierignore` (dist, public, lock, données,
  `testdata`/`fixtures` JSON, `*.min.*`, Markdown, HTML, YAML).
- **Commandes** :
  - JS, JSX, MJS, CSS, JSON de config : `npm run format` (= `prettier --write .`),
    contrôle `npm run format:check`.
  - Python : `ruff format --line-length 100 <fichiers>`.
  - Go : `gofmt -w` ; `gofmt -l` doit rester vide. Pas de `golines`.
- **Largeur 100.** Au-delà, seulement ce qui ne se casse pas (une longue chaîne,
  un gabarit de texte, une URL).
- **Une instruction par ligne.** Pas de `a = 1, b = 2` pour enchaîner des actions,
  pas de `if (x) a(); else b();` sur une ligne, pas de ternaires imbriqués au-delà
  de deux niveaux : écrire un `if` ou une petite fonction nommée.
- **Formater à part.** Un commit `style: format …` ne contient que la sortie du
  formateur ; la logique change dans un autre commit.
- **Les shaders** (WGSL, GLSL) vivent dans des gabarits `` `…` `` : le formateur
  n'y touche pas ; les mettre en forme à la main, une instruction par ligne.

### Glossaire

Un mot par idée, dans le code, les commentaires et les docs :
`core/docs/glossary.md` (le seul glossaire des quatre dépôts). Les
renommages se font au fil des lots, quand on touche le fichier, jamais en
masse.

## Quand s'arrêter

- Les tests rougissent et la cause n'est pas évidente en dix minutes : annuler le
  changement (`git restore`), noter le cas, passer au suivant.
- Le nettoyage demande de changer un comportement observable : le sortir du lot
  de nettoyage et décrire le nouveau contrat, les garanties et ses tests. Si cette
  évolution est déjà autorisée par la demande en cours, la traiter dans un commit
  distinct ; demander seulement si une décision utilisateur manque réellement.
- Le diff dépasse ~400 lignes pour un seul commit : couper.

## Après

Relancer `indexus-audit`, comparer avec le « avant », et donner dans le compte
rendu : lignes retirées par couche, violations de couches restantes, tests.
Mettre à jour `indexus-layers/references/budgets.tsv` si une couche a changé de
nom ou atteint sa cible.
