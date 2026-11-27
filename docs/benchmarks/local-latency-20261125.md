# Petits benchmarks locaux — avant / après

Comparaison du 25 novembre 2026 : `6c1f58e` (main avant permissions locales) contre `a15284d` (permissions locales qualifiées). Le protocole de production n'a pas été modifié dans cette passe.

## Méthode

Quatre passages **avant / après / après / avant**, séquentiels, GOMAXPROCS=4, Go 1.26 sur Apple M1 Pro. Même harnais Go, mêmes données et paramètres. Fichiers et fsync réels, écritures groupées avec délai 1 ms, huit workers. Les RPC entre pairs traversent JSON en mémoire avec RTT artificiel configuré 1 ms (les timers peuvent dépasser ce délai). Aucun TCP/QUIC ni AWS n'est mesuré ici.

Deux cas sans transfert : 400 écritures offertes à 150/s, soit une seule zone ou une nouvelle zone par écriture. Lectures suivantes :1 000 ordinaires et1 000 fraîches par run. Cas join : 1 024 zones semées avec 1 Kio de texte JSON par record, puis 600 écritures à 100/s pendant l'arrivée du second vnode et 150 supplémentaires après le join. Les écritures touchent des zones existantes. Le texte fait 1 024 octets ; l'enveloppe JSON et les autres champs s'ajoutent.

Les latences incluent les retries ; le temps depuis l'arrivée prévue conserve aussi l'attente du générateur. Les quantiles sont nearest-rank sur les échantillons bruts regroupés, jamais une moyenne des p99. Les erreurs sont conservées séparément : aucun échec dans les quatre runs, aucun retry applicatif. Les lectures vérifient le total de tous les seeds et offres ; ce n'est pas un nouvel audit exhaustif par ID ou de tous les écrivains.

Il n'y a que deux répétitions par version : pas d'intervalle de confiance et peu d'observations dans certaines queues. La phase during dépend de la durée du join, donc ses effectifs diffèrent. La ligne join/write/all porte sur la même charge totale dans les deux versions et constitue la comparaison principale.

## Mesures

Tous les triplets ci-dessous sont **p50 / p95 / p99 en millisecondes**.

| Scénario / opération / phase | n avant / après | Avant | Après |
| --- | ---: | ---: | ---: |
| distinct-zones · read-fresh · after | 2000 / 2000 | 1.432/1.592/1.673 | 1.375/1.570/1.678 |
| distinct-zones · read-ordinary · after | 2000 / 2000 | 0.047/0.131/0.230 | 0.049/0.125/0.197 |
| distinct-zones · write · steady | 800 / 800 | 6.066/10.090/13.572 | 5.969/10.087/13.534 |
| join · read-fresh · after | 600 / 600 | 11.167/12.469/12.857 | 11.096/12.407/12.899 |
| join · read-ordinary · after | 600 / 600 | 11.173/12.457/12.655 | 11.109/12.424/12.608 |
| join · write · after | 1172 / 1057 | 7.079/10.157/13.265 | 7.023/10.144/12.056 |
| join · write · all | 1500 / 1500 | 7.047/15.660/27.704 | 7.037/15.458/27.766 |
| join · write · before | 82 / 81 | 5.980/7.284/17.243 | 5.885/7.583/9.603 |
| join · write · during | 246 / 362 | 8.962/31.435/97.486 | 7.970/26.357/62.303 |
| same-zone · read-fresh · after | 2000 / 2000 | 0.352/0.671/0.748 | 0.354/0.679/0.761 |
| same-zone · read-ordinary · after | 2000 / 2000 | 0.023/0.052/0.112 | 0.030/0.067/0.177 |
| same-zone · write · steady | 800 / 800 | 6.298/10.327/15.268 | 6.533/11.559/14.594 |

Sur la charge entière du cas join : p99 depuis l'arrivée prévue de 29,592 ms avant et 28,394 ms après. Les p99 d'exécution de 27,704 ms et 27,766 ms sont pratiquement identiques. Le p99 conditionnel pendant le join baisse de 97,486 à 62,303 ms, mais l'échantillon est plus grand et la fenêtre plus longue après : il ne faut pas transformer cette ligne en gain global ni en durée de gel.

## Ce que l'on détecte

1. **Premier droit natif plus tôt, fin de join plus lente.** Moyennes de deux runs : premier ACK d'installation régionale de 1 215,988 à 494,496 ms (environ −59 %) ; fin du join de 1 232,826 à 1 806,795 ms (environ +47 %). Les instants sont les réponses observées depuis le début du join, pas une instrumentation du commit exact sur disque. Le premier reply de point passe de 161,469 à 182,476 ms. Le premier ACK d'écriture chez le receveur varie de 201 à 430 ms avant et de 389 à 413 ms après : aucun gain démontré sur cette dernière mesure.
2. **Surcoût de fragmentation visible.** Le petit transfert est consolidé en trois grains natifs après, contre une opération régionale avant. En moyenne, 13 RPC install-zone contre 9. Cela explique une partie du travail supplémentaire et constitue une piste à profiler pour réduire le coût de clôture sans retarder le premier grain. Le trafic total sous charge est proche : 4,608 Mo avant contre 4,470 Mo après. Les requêtes install-zone augmentent néanmoins de 590 ko à 708 ko ; la compacité des preuves reste à surveiller.
3. **Petite régression de lecture ordinaire sur zone unique.** Médiane de 23 à 30 µs, p99 de 112 à 177 µs, contre une stabilité générale des lectures fraîches. Un microbenchmark de 64 records profilé séparément mesure de 10,763 à 11,361 µs/op et de 232 à 242 allocations/op (+560 octets). Le contrôle de complétude participe au chemin ; le profil est dominé par le runtime et ne permet pas de lui attribuer seul tout l'écart. Piste : éviter les consultations redondantes des snapshots de routage/complétude, en conservant les garanties de lecture.
4. **Ancien nom de RPC dans les rapports.** report.py et live.py comptaient encore exchange pour le trafic dédié de contrôle. Le nouveau RPC membership était sous-compté. Corrigé, avec étiquetage explicite des archives Knowledge. Le compteur historique knowledge_bytes compte les RPC portant du contrôle, parfois aussi des données : ce n'est pas une mesure des seuls octets du paquet Knowledge.
5. **Latence prévue insuffisamment restituée.** Le rapport exposait les percentiles de service mais omettait la série prévue ; les p95 des lectures manquaient. Ajout des p50/p95/p99 de service et prévus, des effectifs, et des p95 de lecture. Le dashboard expose aussi les trois quantiles. Les histogrammes AWS restent des bornes supérieures de buckets log2, pas les quantiles bruts exacts du test local.

6. **La fin du drain pouvait manquer dans les histogrammes.** Le loader de référence ancien ne produit même pas de bilan final ; le plus récent produit les comptes, mais omet le reliquat des histogrammes après les dernières mesures périodiques. Le générateur commun reçoit une instrumentation minimale (`loader_tail.py`) qui écrit ce reliquat après la fin des workers. Le rapport l’ajoute une seule fois et indique si ses distributions couvrent tous les ACK finaux. La modification ne touche pas les sources des nœuds et son empreinte est consignée. Les loaders instrumentés des deux révisions compilent pour Linux.
7. **Le générateur avait lui-même changé.** L’ancien incrémente les arrivées d’une quantité fixe par tick ; le nouveau tient compte du temps écoulé et apprend les destinations par zone. Les utiliser séparément biaiserait une comparaison AWS. Le lanceur compile donc le même générateur récent pour les deux protocoles ; les scripts d’infrastructure et d’analyse sont également communs. Cela n’a pas affecté les essais locaux, qui utilisaient déjà un harnais identique.

## Vérifications de l'outillage

Les quatre campagnes normales passent. Harnais validé sous -race sur zone unique et join. La première vérification join sous -race a atteint le budget fixe de 90 s uniquement pendant les lectures finales, sans avertissement de race ; le budget utilise désormais RaceScale et la reprise instrumentée passe en 120,90 s. Ces latences instrumentées sont exclues de la comparaison.

Quatorze tests Python sur report/live/evidence et dix tests de capacité AWS passent. Compilation linux/amd64 sans CGO réussie pour node/issuer/provisioner/experiment et node H3, sur les deux versions. Cela vérifie la construction, pas le démarrage d'EC2.

La source mesurée exacte est conservée dans le dossier brut avec son SHA. Après mesure, seuls le budget de temps sous -race et le cleanup en cas d'arrêt ont été renforcés dans le harnais ; le parcours normal est identique. Aucun changement du protocole ni optimisation sur la base de quelques échantillons bruités.

## Reproduction et préparation AWS

Créer un checkout détaché propre de `6c1f58e` puis lancer :

```sh
python3 scripts/research/compare-latency.py --before /chemin/avant --after /chemin/apres --out /nouveau/dossier
```

Le script copie seulement le test de mesure dans le checkout avant. Il refuse d'écraser un test différent et conserve sorties, erreurs et manifest. Le checkout temporaire utilisé dans cette campagne a été supprimé ; les résultats restent archivés.

Le lanceur AWS est `ops/experiment/aws/compare-permissions.sh` : **--plan par défaut ne fait aucun appel AWS**. --preflight lit tarifs, vCPU disponibles et capacité effective ; --run provisionne, uniquement sur action explicite. Les données et clusters sont neufs par version. Le checkout de chaque run est conservé pour les logs et la récupération de son état.

```sh
bash ops/experiment/aws/compare-permissions.sh --plan smoke before
bash ops/experiment/aws/compare-permissions.sh --plan smoke after
bash ops/experiment/aws/compare-permissions.sh --preflight smoke after
# Plus tard seulement, après revue du préflight :
bash ops/experiment/aws/compare-permissions.sh --run smoke before
bash ops/experiment/aws/compare-permissions.sh --run smoke after
```

Smoke : 250 → 500 → 1 000 → 2 000 w/s, deux hosts initiaux × deux slots, au moins quatre hosts possibles, rampe 15 s / plateau 30 s / tail 45 s, TTL 20 min, enveloppe 3 USD **par run**. Le profil 16k : 2 000 → 4 000 → 8 000 → 16 000 w/s, au moins seize hosts possibles, rampe 60 s / plateau 120 s / tail 180 s, TTL 45 min, enveloppe 6 USD **par run**. Ce sont des enveloppes de planification, pas une estimation de dépense ni un coupe-circuit de facturation. Les tarifs ne sont pas codés en dur ; un budget insuffisant doit être relevé explicitement.

Les profils isolent la demande d’écriture : `ITEMS=0`, `WRITE_CAP=0`, `SCALING_WRITES=250` pour smoke et `500` pour 16k. Cela évite de confondre autoscale sur volume accumulé et autoscale sur débit demandé.

Le plafond opérateur est 64 machines, réduit par budget/quota/autres instances ; si la capacité effective est inférieure au minimum du profil, le préflight refuse avant toute création. Les quotas/prix n'ont pas été interrogés pendant cette préparation. Le préflight ne réserve pas la capacité : conserver aussi capacity.json réel du run. Les nœuds/issuer/provisioner viennent de chaque révision ; le générateur et l’infrastructure de mesure sont communs. `LOADER_REF` permet de figer la même révision de générateur pour les deux passages. Le runner consigne les commits du protocole, du générateur et de l’infrastructure, ainsi que le diff du générateur. Le même analyseur actuel retraite ensuite les deux sorties. Le dashboard p50/p95/p99 s’ouvre pendant la campagne ; un live-percentiles.html homogène est reconstruit pour chaque phase à la fin. Les courbes restent des relevés périodiques ; le bilan couvre aussi le reliquat après drain.

`STEP_ROUTE=relay` permet une seconde paire après la paire direct. Les campagnes doivent rester séquentielles, avec teardown terminé et audit des ACK avant la suivante. Vérifier ACK/offres, erreurs/shed, compteurs de capacité/quota, CPU du loader, files / intended latency, transferts ouverts en fin de tail, totaux, octets par kind et snapshots des transferts bloqués. Le minimum de 32 vnodes possibles du profil16k est un choix de dimensionnement, pas une preuve que 32 vnodes soutiendront 16k/s.

Le premier bilan de la charge doit distinguer les échecs de tentatives HTTP (qui peuvent être rejouées) des opérations finalement épuisées : `load_final.exhausted`. Exiger `drain_completed=true`, `queued_unfinished=0`, ACK finaux égaux aux offres sans shed, audit vrai, et `latency_covers_final_acks=true` avant d’utiliser les p99 comme distribution complète. Les compteurs périodiques sont conservés séparément des comptes finaux.

**Aucune machine AWS lancée, aucun appel de tarification ou quota effectué dans cette passe.**


Résultats structurés : [comparaison](results/local-latency-20261125.json).
