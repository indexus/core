# Déplacer un vnode entier sur le palier AWS

**Dans le rejeu, aucune décision ne réunit une source à moins de 60 % CPU, un voisin disponible et une charge durable à déplacer.** Le trafic justifierait un déplacement chez un donneur déjà saturé. Le choix porte donc autant sur le moment de démarrer que sur l’adresse du receveur.

## Fenêtre observable

Dans le fichier local `aws-place-100-direct-1127/live.jsonl`, le rejeu décide entre 540 et 630 s, avec des observations futures jusqu’à 660 s. Un donneur dépasse 50 % CPU pendant au moins 15 s, avec deux valeurs de CPU distinctes. Les observations espacées d’environ cinq secondes peuvent compter plusieurs fois un même hôte. On exclut une machine où une copie est visible. Les voisins candidats sont les couples de machines qui ont échangé un transfert dans les 300 s précédentes. Cette approximation du voisinage peut omettre des pairs légitimes et ne prouve pas que chaque vnode connaît chaque pair de sa machine.

Le candidat doit offrir un emplacement libre, rester sous 60 % de RAM et disque et disposer de la marge CPU pour le budget d’admission de réception de 1,75 cœur, soit 43,75 points sur quatre vCPU. Après une décision, chaque machine du couple attend 45 s avant une nouvelle proposition. Chaque proposition est une expérience indépendante sur la topologie observée. Les déplacements hypothétiques ne modifient pas les événements ultérieurs d’AWS.

| Règle côté source | Observations en pression | Source en copie | Source au-dessus du budget | Source en délai de garde | Aucun voisin admissible | Propositions |
|---|---:|---:|---:|---:|---:|---:|
| Budget de réception seul | 39 | 9 | — | 9 | 19 | 2 |
| Source à 60 % au plus avant copie | 39 | 9 | 28 | 0 | 2 | 0 |
| Source à 60 % avec marge hypothétique de 0,25 cœur pour copier | 39 | 9 | 30 | 0 | 0 | 0 |

## Trafic futur

À 542 s, le donneur `machine-26` est à 100 % CPU. La règle fondée sur les applications récentes sélectionne le vnode `Bu0W8fQvOD-1JnpE` (84 778 items) et le voisin `machine-06` (2,75 % CPU). Le trafic du vnode représente 506 applications/s dans la fenêtre de +10 à +30 s. Le donneur en supporte 1 297/s et la cible 119/s. Si l’autorité sur tout le vnode avait déjà basculé et que la même demande avait été servie, la charge se répartirait à 791 et 625/s. C’est une estimation de répartition des applications. Le délai réel de copie, les ACK, la demande refusée, le CPU résultant et le débit réseau restent inconnus. Un transfert réel survient sur le couple dans cette fenêtre.

La règle fondée sur le stock choisit le même vnode et une autre machine, `machine-15`, à 11,75 % CPU. Elle aurait reçu 730 applications/s au total et le pic de la paire aurait également été 791/s. Une seule fenêtre vérifiable ne départage pas les deux stratégies. À 588 s, le donneur est encore à 73,5 % CPU. À 608 s, le débit d’un vnode de la cible manque. La fenêtre future de cette seconde proposition est donc incomplète.

Les deux mesures comparées utilisent la moyenne des applications observées par vnode. Pour une source S, une destination D et un vnode V, le gain est la différence entre max(S,D) et max(S−V,D+V). Les compteurs de débit de la fenêtre future servent uniquement à évaluer une décision déjà prise.

## Coût transitoire mesuré séparément

Dans le laboratoire local isolé, sur trois répétitions, un MOVE de 2 048 enregistrements coûte en médiane 10,27 s de temps écoulé, 11,91 s de CPU cumulé, +33,11 Mio de pic de tas et +20,23 Mio sur disque, donneur et receveur confondus. Sur 256 enregistrements, les médianes sont 0,29 s, 0,20 s CPU, +4,55 Mio et +1,11 Mio. La répartition des zones compte aussi. La variante étalée de 2 048 enregistrements a pris 7,72 s et +9,34 Mio sur disque. Les 84 778 items de la trace se situent très au-delà des tailles mesurées. Il serait hasardeux de chiffrer un temps de copie AWS, une part de cœur côté source ou une économie CPU à partir de ces expériences.

Pour l’essai suivant, mesurer la pente de la demande du vnode, la marge CPU encore disponible sur le donneur et le coût source d’un lot pendant la copie permettrait de tester un départ anticipé. Une machine distante disponible, à elle seule, ne garantit aucune capacité de copie chez un donneur à 100 %.
