---
name: indexus-resources
description: Budget de ressources d'Indexus — disque, mémoire, processeur et GPU du nœud, du SDK et d'Explore. À utiliser avant d'ajouter une collection, un cache, une boucle d'arrière-plan, une couche de rendu ou un import, et quand on demande de mesurer, d'alléger ou de vérifier que « ça tient » sur un portable.
---

# Tenir le budget

Indexus doit tourner sur un portable (16 Go, GPU intégré) sans le saturer.
Chaque ajout annonce son coût et le mesure. Un chiffre non mesuré s'écrit
« estimé ».

## Les plafonds

| Ressource | Plafond | Comment le vérifier |
|---|---|---|
| Disque du nœud | ≤ 1,5 × la donnée brute | octets du dossier du nœud ÷ octets des records importés |
| CPU du nœud au repos | < 2 % d'un cœur après convergence | `ps -o %cpu -p $(cat _run/.data/pids/node1.pid)`, 60 s sans lecteur |
| Mémoire d'un nœud | plafond de config (256–512 Mo) | RSS sous charge de lecture |
| Écriture | ≥ 20 000 records/s par lot | banc d'import |
| Vue d'Explore | octets et éléments constants quel que soit le zoom | compteurs de la couche, vues large / loin / mi-distance / près |
| Image d'Explore | ≤ 8 ms GPU, ≤ 4 ms sur le fil principal | sonde `?profile3d`, volet visible |
| Mémoire GPU par couche | tampons dimensionnés par le budget, jamais par la donnée | taille des tampons à la création |

Un plafond dépassé est un défaut : on corrige ou on le signale, on ne le
relève pas en silence.

## Règles

1. **Rien ne tourne quand rien ne change.** Une boucle d'arrière-plan part d'un
   ensemble « à faire » ; vide, elle dort. Pas de balayage complet périodique.
   Les réessais reculent (délai doublé, borne, aléa) et s'arrêtent.
2. **Tout cache a une taille.** Octets, pas nombre d'entrées ; valeur par défaut
   pour un portable ; clé de config ; éviction du moins récemment utilisé.
3. **Une donnée, une copie.** Sur disque comme en mémoire : pas de seconde
   forme « pratique » du même record, de la même somme, du même bloc.
4. **Le budget, pas la donnée, fixe le coût d'une vue.** Raffinement du plus
   visible d'abord jusqu'à épuisement d'un budget fixe d'éléments ; pas de
   table par zoom ; ce qui n'est pas chargé dessine son parent.
5. **Au-dessus de l'objet, des sommes ; en dessous, du détail.** Une collection
   donne ses vues lointaines par les sommes de l'arbre. Les payloads lourds
   (maillages, points, tuiles) ne se lisent que près de la caméra, par des
   records ou des emplacements que les lectures lointaines ne touchent pas.
6. **Petit d'abord.** Un essai se fait sur 200 m, volet visible, puis 1 km.
   Avant un import : nombre de records × octets, et le temps prévu.
7. **Les essais ne restent pas.** Une collection d'essai remplacée est retirée
   du nœud le jour même ; les fichiers d'import intermédiaires aussi.
8. **On arrête proprement.** `explore.sh stop`, puis vérifier qu'aucun
   processus ni port Indexus ne reste. Fermer les onglets d'Explore : un onglet
   caché continue de solliciter le GPU.

## Avant de livrer

- Le tableau avant / après des lignes concernées ci-dessus.
- Ce qui a été mesuré, où (machine, volet visible ou non), et ce qui est estimé.
- Ce qui reste au-dessus d'un plafond, dit en première ligne.
