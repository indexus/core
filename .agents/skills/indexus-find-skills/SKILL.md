---
name: indexus-find-skills
description: Trouver, évaluer et installer un skill externe (skills.sh, GitHub) adapté à Indexus, ou décider d'en écrire un. À utiliser quand on demande s'il existe un skill pour une tâche, quand une tâche revient pour la troisième fois, ou quand on aborde une techno que les skills du projet ne couvrent pas (WGSL, Metal, QUIC, MapLibre, Terraform…).
---

# Chercher un skill pour Indexus

Les skills du projet vivent dans `core/.agents/skills/` : une seule copie,
versionnée avec le nœud. À la racine du dossier de travail, `.agents` est un
lien vers `core/.agents` (lu par Codex, Cursor, Gemini) et `.claude/skills`
un lien vers `core/.agents/skills` (Claude Code) ; voir le `AGENTS.md` de
cette racine.

## Méthode

1. **Dire le besoin en une ligne**, avec la couche concernée (`indexus-layers`).
2. **Chercher** : `npx skills find "<mots-clés>"`, puis le classement de
   https://skills.sh. Mots-clés utiles ici : `webgpu`, `wgsl`, `go refactor`,
   `go concurrency`, `code simplification`, `dead code`, `maplibre`, `react
   performance`, `terraform`.
3. **Filtrer** : au moins ~1 000 installations ou un éditeur reconnu ; licence
   permissive ; mis à jour depuis moins d'un an ; pas de scripts qui appellent le
   réseau ou lisent hors du dépôt.
4. **Lire le SKILL.md en entier, et ses scripts, avant d'installer.** Un skill
   est du texte qui pilote l'agent : on ne l'installe pas sans l'avoir lu.
5. **Installer dans le projet, jamais en global** :
   `npx skills add <owner/repo> --skill <nom> -a codex -y` (ajouter
   `-a claude-code` au besoin).
6. **Vérifier qu’il ne contredit pas** `indexus-layers`, `indexus-simplify`, `indexus-resources` ni
   `indexus-gpu-path`. En cas de conflit, les skills `indexus-*` priment ; noter
   la divergence en tête du skill installé, ou ne pas l'installer.
7. **Rien de convaincant ?** Écrire un skill `indexus-<sujet>` court (une page),
   avec une description qui dit *quand* l'utiliser.

## Présélection du 2026-11-13 (repérée sur leurs fiches, pas lue en entier, non installée)

| Besoin | Skill | Commande |
|---|---|---|
| Chercher des skills | `vercel-labs/skills` → `find-skills` | `npx skills add vercel-labs/skills --skill find-skills -a codex -y` |
| Simplification générique (complète `indexus-simplify`) | `addyosmani/agent-skills` → `code-simplification` | `npx skills add addyosmani/agent-skills --skill code-simplification -a codex -y` |
| WebGPU / WGSL bruts, compute et rendu | `cazala/webgpu-skill` → `webgpu` (peu d'installations : à relire) | `npx skills add cazala/webgpu-skill --skill webgpu -a codex -y` |

Écartés : `dgreenheck/webgpu-claude-skill` (Three.js TSL, Indexus n'utilise pas
three) ; les skills « refactor » à moins de 200 installations.
