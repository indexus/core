# Archive

Historical notes kept for context. They describe the code as it was when they
were written and are **not** maintained: paths, formats and collection names
in them may no longer exist. The current documentation is indexed in
[../README.md](../README.md).

| File | What it was |
| --- | --- |
| [storage-interface-plan.md](storage-interface-plan.md) | Plan of the cut from bbolt to zonelog (executed; the node now stores everything in zonelog) |
| [experience-organization-2026-11.md](experience-organization-2026-11.md) | Layout of the apps before they were merged into the `experiences` repository |
| [sdk-alignment-2026-11.md](sdk-alignment-2026-11.md) | Report of the move of every app onto the SDK (content-addressed assets, since replaced by native collections) |
| [campaign-2026-11-14.md](campaign-2026-11-14.md) | A/B campaign of `cmd/benchmark` (bbolt era, harness since archived outside the repository) behind the linear-knowledge work |
| [replication-majority-2026-11.md](replication-majority-2026-11.md) | Contract REP-1 to REP-7 of the majority-journal replication, replaced by the convergent design; its model and journal were removed (git `7379aaf`) |

Older reports (benchmark campaigns, audits, the former quorum design) are
kept outside the repositories and are not needed to work on the code.


## Prédécesseur à checkpoints : instantanés du 25 novembre 2026

- [Knowledge et ses mesures](knowledge-frontier-20261125.md) : ancien transport
  full/ref/delta, attente de porteur et comparaisons mesurées.
- [Transfert de zone](transfert-de-zone-frontier-20261125.md) : carte des étapes
  du code avant remplacement par tickets locaux.

Ces instantanés expliquent les décisions et résultats passés. Le
[contrat cible](../protocol/local-permissions.md) et sa
[qualification](../protocol/local-permissions-verification.md) portent le statut
réel du remplacement ; l'archive ne décrit pas un mode legacy à conserver.
