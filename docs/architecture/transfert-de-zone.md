# Transfert de zone : copier, geler un lot, installer

**Statut : remplacement branché dans le worktree, qualification complète en cours, 25 novembre 2026.**
Le [contrat détaillé](../protocol/local-permissions.md) et sa
[matrice de qualification](../protocol/local-permissions-verification.md)
distinguent les scénarios déjà testés de la qualification encore en cours.

Le nouveau vnode n'attend pas la copie régionale entière. La source copie à
chaud, puis bascule de petits lots. Les descendants passent avant le parent ;
la chaleur priorise les zones à profondeur comparable. Une priorité décide de
l'ordre, jamais du droit d'écrire.

1. La source réserve une opération unique en restant écrivain. Le receveur
   alloue un ticket tentatif ; la première copie confirme ce ticket avec ses
   premiers octets. La source persiste cette confirmation avant tout gel.
2. La copie et le rattrapage chaud précèdent la fence. Le lot gelé reçoit son
   dernier delta, son sceau et son autorisation paire à paire.
3. Le receveur installe les octets et la permission ensemble. Il peut acquitter
   ce lot immédiatement ; ni fin du join ni diffusion globale ne le conditionnent.
4. La région est consolidée sous le même ticket et identifiant de droit, sans
   écraser les ACK déjà reçus sur ses points. L'espace vide est inclus.
5. La source collecte après l'ACK durable. Les renvois nécessaires aux lecteurs
   restent disponibles et migrent avec les exceptions actuelles.

Après la **première fence**, l'opération se reprend ; elle ne s'annule pas sur
un timeout. Le fait que la région entière n'ait pas encore été émise ne rend pas
l'annulation sûre : un de ses points peut déjà être installable chez le receveur.
Le [modèle d'annulation](../../spec/handoff/FirstFenceCancellation.tla) et son
contre-exemple isolent précisément cette erreur.

Le gel est l'intervalle fence→installation du lot. Il n'inclut pas la copie
chaude ; il reste sensible au réseau, aux fsync et à la saturation du receveur.
La règle algorithmique est l'absence de travail proportionnel à l'histoire ou
d'accord global après la fence, sans promettre une durée fixe en millisecondes.

L'[instantané précédent](../archive/transfert-de-zone-frontier-20261125.md)
conserve l'ordonnancement et les paramètres de l'ancien code. Les noms de ses
certificats et sa dépendance au placement ne définissent pas la cible locale.
