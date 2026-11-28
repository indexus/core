---
name: indexus-layers
description: Charte des couches d'Indexus (arbre → autorité → stockage → échange → parcours → GPU → bords). À lire avant de créer, déplacer, fusionner ou renommer un fichier, un paquet Go ou un module JS, et avant toute décision d'architecture dans core, sdk, dashboard ou experiences.
---

# Les couches d'Indexus

## L'idée, en une phrase

Un espace à N dimensions est projeté dans **un seul arbre binaire**. Tout y a une
adresse : un nœud, une collection, une zone (set), un item. La proximité XOR indique le placement souhaité des zones. Le droit installé
sur le disque détermine qui peut écrire pendant et après leur transfert. Le client descend le même arbre, du plus
proche au plus lointain. Tout le reste est au service de cette phrase.

Test à appliquer à chaque fichier : **« quelle couche de l'arbre est-ce que je sers ? »**
Si la réponse est « aucune » ou « deux », le fichier est mal placé ou à couper.

## Les sept couches (mêmes noms côté nœud et côté client)

Une couche n'importe que les couches au-dessus d'elle dans ce tableau.

| # | Couche | Question | Nœud Go (`core`) | Client JS (`sdk/src`) |
|---|--------|----------|------------------|--------------------------|
| 1 | **tree** | Où est-ce dans l'arbre ? | ID, ZoneKey, Region, View/Owner, trie XOR, encodage `Base`. Pur, sans I/O. | Location (bits + profondeur), masque, distance, abélien, un seul alphabet. |
| 2 | **authority** | Qui a le droit d'écrire ? | `internal/causal` (droits locaux, tickets et membership compatible convergente). | — (le client n'a pas d'autorité) |
| 3 | **store** | Où sont les octets ? | `internal/zonelog` (le moteur) et `internal/storage` (fences, reçus, état de contrôle). | `src/store` : un cache unique, blocs IXB2 par zone, un budget en octets. |
| 4 | **wire** | Comment ça circule ? | `internal/mesh` (pair↔pair) + IXB2 / IXRB servis au client (`indexus`). | `src/wire`, `src/network`, `src/api` : transport `fetch`, redirections, décodeurs. |
| 5 | **walk** | Dans quel ordre parcourir ? | Projection de lecture, résumés agrégés. | `src/walk` : une descente, deux politiques : distance (`Local`), viewport (`walkViewport`). |
| 6 | **gpu** | Comment calculer et dessiner ? | — | `src/gpu` : un device, arène, pack, champ, combine, raster ; une frame par device. |
| 7 | **bords** | Qui s'en sert ? | `indexus` (façade), `adapter`, `app`, `cmd`. | `map`, `react`, puis `dashboard` et `experiences`. |

## Règles

1. **Un concept, un nom, un endroit.** Avant d'ajouter un type, chercher s'il existe
   déjà (`rg`). Vocabulaire fixé : `core/docs/glossary.md` (*zone*, *record*,
   *Sum*, *Block*, *Cell*, *region*, *view*…).
2. **Le contrat `tree` est le même en Go et en JS.** Tout changement d'encodage se
   fait des deux côtés dans le même travail, avec des vecteurs de test partagés.
3. **Les bords passent par la façade.** Un adaptateur HTTP ou un binaire `cmd/`
   n'importe ni `mesh`, ni `causal`, ni `storage`. Une app n'importe que
   `js-indexus-sdk`, `/map`, `/gpu`, `/react`.
4. **Pas de métier dans le SDK.** DVF, météo, styles de carte vivent dans les apps.
5. **Pas de code « au cas où ».** Un mécanisme non branché en production vit dans
   les tests ou dans les archives (`docs/archive/` ou hors des dépôts), pas dans
   un paquet de production.
6. **Budget de lignes.** Chaque couche a une cible dans `references/budgets.tsv`.
   Un changement qui fait grossir une couche au-delà de sa cible doit en retirer
   autant ailleurs, ou justifier l'écart dans le message de commit.
7. **Garde-fous exécutables.** Toute règle de dépendance ajoutée ici s'ajoute aussi
   à `core/internal/architecture/boundaries_test.go` ou à
   `indexus-audit/scripts/audit.sh`.

## Arborescence actuelle

```
core/
  internal/tree/        adresses : ID, régions, View/Owner, trie XOR, Base
  internal/causal/      topologie CRDT, autorité, certificats
  internal/protocol/    capacité, plans, handoff, forwarding (sans I/O)
  internal/zonelog/     le moteur de stockage
  internal/storage/     l'état durable du nœud sur zonelog
  internal/mesh/        échanges entre pairs, handoff, reprise
  internal/prep/        format de batch IXP1
  indexus/              façade : lectures, écritures, résumés, bulk, fil client
  internal/adapter/ internal/app/ cmd/   bords
  tools/{localdata,prep}                 outils hors ligne

sdk/src/
  tree/                 adresses, Space, sommes abéliennes
  wire/ network/ api/   fil, transport, Network
  store/                blocs par zone
  walk/                 descente de l'arbre
  gpu/                  device, arène, noyaux
  map/ react/           bords (carte, pipeline de chaleur, hook React)
  local/ runtime/ utilities/   lecture « Nearby », sessions, outils
```

Une couche à la fois, de haut en bas. Voir `indexus-simplify` pour la
méthode ; `references/audit-2026-11-13.md` est l'audit de départ de la
charte (avant la vague 3), gardé comme repère historique.

## Contrats à préserver

Les garanties, les formats IXB2/IXRB/IXP1 et leurs vecteurs partagés, les
frontières de couches, l'annulation et la soumission GPU unique restent des
contrats à préserver. Ce ne sont pas des fichiers intouchables : une
implémentation de `Region`, de l'index XOR ou du stockage peut être simplifiée
ou optimisée si ses contrats restent vérifiés. Une évolution explicite de contrat
se décrit et se teste séparément du nettoyage. La documentation distingue les
anciens certificats globaux des droits/tickets locaux réellement utilisés.

Pour une lecture documentaire pilotée par le SDK, utiliser la [fiche de
qualification des lectures locales](references/lectures-locales.md). Elle
précise comment vérifier le travail du pair et la portée de la réponse.
