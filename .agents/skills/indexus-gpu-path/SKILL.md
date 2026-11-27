---
name: indexus-gpu-path
description: Règles du chemin de données de bout en bout d'Indexus, du disque du nœud au GPU du client — format de fil en colonnes, TypedArray, GPUBuffer, compute et rendu WebGPU/WGSL. À utiliser dès qu'on touche au format /sets ou /records, au décodage côté SDK, aux caches, aux workers, à sdk/src/gpu ou map, ou à une couche de rendu dans dashboard ou experiences.
---

# Du disque au GPU

Objectif : un item lu sur un nœud arrive dans un `GPUBuffer` **sans jamais devenir
un objet JS**. Une zone de l'arbre = un bloc d'octets = un tampon.

## Le chemin cible

```
zonelog (record IXR1, sommes par zone dans l'index) → bloc IXB2 par zone →
HTTP (gzip) → ArrayBuffer → vues TypedArray → store → arène GPU →
pack / champ / combine → rendu, une frame par device
```

## Règles

1. **Le bloc est l'unité.** Un bloc = une zone : en-tête fixe (profondeur, n,
   nombre de colonnes, version de zone), puis colonnes contiguës alignées sur
   4 octets, little-endian. Clés en entier (`u32`/`u64`, chemin de bits), pas en
   caractères. `count:u32`, `metrics:f32`.
2. **Zéro objet par item.** Interdits sur le chemin chaud : `JSON.parse`,
   `TextDecoder` par ligne, `{count, metrics}` par item, `Map` à clé chaîne par
   item, `Object.entries`. On lit avec `new Uint32Array(buf, off, n)`.
3. **La compression est celle du transport** (zstd/gzip), pas un compactage en
   bits maison qui impose un décodage valeur par valeur.
4. **Un seul cache côté client**, indexé par zone, qui garde les blocs tels
   qu'ils sont arrivés. Les couches du dessus gardent des vues, pas des copies.
5. **Ce que le GPU peut faire, le CPU ne le fait pas** : culling par viewport,
   choix du LOD, agrégation/binning, tri par distance, projection, masque de
   polygone. Le worker ne fait que réseau, cache et envoi.
6. **Pas d'aller-retour GPU → CPU → GPU.** Un résultat de compute consommé par
   le rendu reste sur le GPU. `mapAsync` est réservé à ce que le CPU doit
   réellement lire (picking, statistiques affichées).
7. **Un seul device, une seule pile.** WebGPU via `gpu/device.js`. Pas de
   programme WebGL : Explore n'a qu'un renderer WebGPU, MapLibre n'y dessine
   que le fond de carte et les labels. Le pont WebGPU → WebGL de MapLibre
   (`webglComposite.js`, mode non « host-drawn » d'`AverageGridLayerWebGPU`)
   ne sert qu'aux cartes dont MapLibre reste l'hôte (dashboard).
8. **Les tampons se transfèrent et se recyclent** (`postMessage` avec transfert,
   `RELEASE_PACKED`). Jamais de copie entre worker et thread principal.
9. **Go et JS changent ensemble.** Tout changement de format : encodeur Go,
   décodeur JS et un vecteur de test binaire partagé, dans le même travail
   (`core/indexus/testdata` ↔ `sdk/src/wire/testdata`,
   `sdk/src/api/testdata`). Une coexistence avec un ancien format n'est requise que si des consommateurs
   déployés doivent réellement être migrés. Sans nœuds en production, garder le
   seul contrat cible, changer les producteurs/consommateurs ensemble et retirer
   les anciens chemins avec leurs tests orphelins.
10. **On mesure avant et après** : octets sur le fil, temps du premier pixel,
    allocations par item (profil mémoire), temps de frame. Pas de gain annoncé
    sans chiffre.

## Où en est le chemin

- Fil : `/sets` en IXB2 seulement, avec sélection de colonnes
  (`columns=`) ; `/records` en JSON ou IXRB v2 (payloads binaires). Contrat :
  `core/docs/protocol/wire-block.md`.
- Nœud : records et état de contrôle dans zonelog, sommes compensées,
  payloads compressés ; aucune dépendance externe (pas de bbolt, pas de migration).
- Client : un store de blocs par zone, une arène GPU par collection, le
  pipeline de chaleur (`sdk/src/map/heat`) et `compose` pour joindre
  jusqu'à 4 champs. Contrats : `sdk/docs/layers.md`,
  `sdk/docs/heat-pipeline.md`, `sdk/docs/compose.md`.
- La seule relecture `mapAsync` d'Explore est celle des temps GPU
  (timestamp-query) pour le HUD, dans `experiences/src/rendering/gpu/engine.js` :
  une statistique affichée, permise par la règle 6.
- `/aggregates` existe côté nœud et le SDK ne l'appelle pas.

L'audit de départ (13 novembre 2026, avant la vague 3) reste dans
`../indexus-layers/references/audit-2026-11-13.md`, comme repère historique.
