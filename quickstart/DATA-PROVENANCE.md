# Sample data provenance — review draft

This document records evidence found on 17–18 November 2026. It is not a release
clearance. Public distribution remains gated on exact source versions,
attribution and transformation lineage.

The current local Docker sample is cross-collection v2 (14 collections,
308,551 records); see the last section. Earlier samples below are retained as
the transformation history, not the current release identity.

## Historical 30 km-weather sample

`indexus-sample-3d-weather-v1.tar.xz`, SHA256
`d773d65c8092d59a7275c4ccf553f8f4a473958bfcc7011268ff0e5e0da45bfd`,
contains 46,839 records in 13 collections. Its local builder crops existing
prepared exports; it does not download the original inputs.

Generate a bounded evidence report without fetching or modifying data:

```sh
python3 scripts/local/inspect-sample-provenance.py \
  --root /path/to/indexus \
  --sample /path/to/scene-weather \
  --out /path/to/provenance-review.json
```

The script reads collection counts and the archive name from `batches/sample.json`,
checks every prepared file against its archived bytes, and checks the January
weather descriptor and retained LiDAR subset hash. It records metadata file
hashes and keeps unknown dates unknown. This is not a proof of every
transformation edge. Optional derivative checks are shown for the current sample
below.

## Evidence and attribution to reconcile

| Family | Local evidence | Remaining gap |
| --- | --- | --- |
| Terrain and vegetation | Retained LiDAR source manifest records IGN attribution, Licence Ouverte 2.0, nine COPC URLs and subset hash `fad255e0…473a07`; that hash matches the raw manifest. | Complete the link from this input through prepared returns and cropped exports. A delivery path dated 2025-09-26 is not each tile's acquisition/update date. |
| Buildings/roofs | Retained BD TOPO WFS metadata identifies IGN, response hash and retrieval on 10 November 2026; reconstruction combines footprints and LiDAR. | Confirm the exact input snapshot used by the prepared building batches, rather than borrowing a later catalogue. |
| Aerial imagery | Retained manifest identifies IGN BD ORTHO, Géoplateforme WMS layer, image hash and retrieval on 10 November 2026. Its acquisition date is explicitly null. | Recover the applicable imagery edition/update date. The collection name containing 2024 is not sufficient evidence. |
| Weather | Archived content manifest is `france-2025-01-28-3d-v1-atmosphere`, first timestamp 1738022400, 72 hours. Atmospheric metadata names Open-Meteo; acquisition scripts read its AROME HD temperature and AROME France 2.5 km S3 archives. | Preserve original archive chunk identities/hashes and acquisition/update dates. Review town labels and land-mask inputs separately. |

The current public-folder weather manifest is a different June 2025 archive. It
must not be substituted as evidence for this January sample. Likewise, current
catalogues naming newer scene collections do not establish the lineage of the
older cropped batches.

## Official references checked on 17 November 2026

IGN identifies BD TOPO, LiDAR HD and BD ORTHO among its open products, while the
BD TOPO dataset page explicitly declares Licence Ouverte 2.0:
[IGN open data](https://www.ign.fr/institut/des-donnees-et-logiciels-ouverts-au-service-de-la-nation),
[BD TOPO metadata](https://www.data.gouv.fr/datasets/bd-topo-r).
These general/current pages do not establish the exact vintage of our files.

The [official Licence Ouverte 2.0 text](https://www.data.gouv.fr/pages/legal/licences/etalab-2-0)
requires source attribution and the reused information's update date, and must
not imply producer endorsement. Retrieval dates and acquisition/update dates
are separate fields.

Météo-France's [AROME product page](https://donneespubliques-int.meteofrance.fr/?fond=produit&id_produit=131&id_rubrique=51)
identifies its direct data under Licence Ouverte and specifies Météo-France
credit. Our local weather inputs also pass through Open-Meteo, so that direct
product page alone is insufficient for their attribution.

[Open-Meteo's data licence page](https://open-meteo.com/en/licence) identifies
CC BY 4.0 for its data and calls for a credit link at the display, a licence
link and identification of changes. Its
[official self-hosting guide](https://github.com/open-meteo/open-meteo/blob/main/docs/getting-started.md)
describes the S3 distribution used by the local acquisition scripts.
[Service/API terms](https://open-meteo.com/en/terms) separately restrict the free
API to non-commercial use. The local scripts read S3 objects; do not infer that
an API subscription is required, or that service terms are irrelevant, without
checking the applicable acquisition channel. No subscription or new data fetch
has been initiated.

## Credit wording to prepare, not a completed notice

- IGN: derived from BD TOPO, LiDAR HD and BD ORTHO; include product links and
  the exact source update/edition dates once established. Identify cropping,
  meshing, resampling and Indexus indexing as our transformations.
- Weather: Météo-France AROME data distributed by Open-Meteo; link to
  [Open-Meteo](https://open-meteo.com/) and
  [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/), identify the
  28–30 January 2025 archived series and our cropping/regridding. Do not call
  it current weather or assert a single 72-hour forecast initialization.

Before release, put the reconciled notice in the archive/image and an effective
weather attribution link near its display. Keep data terms separate from the
software's proposed Apache-2.0 migration. The January archive, runtime and UI
were not changed by this review.


## Local France-temperature derivative, 17 November 2026

`indexus-sample-3d-weather-france-v1.tar.xz`, SHA256
`89f4cf82fd4e5ce4517ffca5f85dc542641471aa01558b1e6a8f88dcb8a54c6d`,
26,000,408 compressed bytes, 64,355 records in 13 collections. This supersedes
the local 30 km-only temperature preview, not the source provenance review.
The 644,741-record prepared temperature input hashes to
`fae5dd5fdb40d011230ad46815156ee2b74419ea9c84a5f491a5c852d115216a`.

The builder retains 1,988 native records in whole coarse cells intersecting
the Anglet 30 km window, and groups the rest into 17,224 actual GPS zone sums
at 12 bits per axis. It keeps all 73 additive metrics and never combines a
coarse record with its native descendants. No new acquisition or interpolation
is used. The other 12 collections are unchanged. `temperature-proof.json`
records source hash/count/sums; all batches pass `prep verify`. The series
still covers 28–30 January 2025, not current weather or overseas France.
Source rights, acquisition dates and release review gaps above remain open.


## Local cross-collection derivative

`indexus-sample-3d-cross-collection-v1.tar.xz`: 26,054,012 bytes, SHA256
`62c740e8e8e13f0fd84d8399eaed79698843f0ecb62c826930c62f6498f0afe5`.
Adds three unchanged DVF 2020 house-sale records from prepared source
SHA256 `9be328aeb1b6ec14f990816fb480664429d9066df43fe0a94573f0aad880ca54`.
The crop is the 300 m Anglet bounding square using geocoded metric coordinates;
no national DVF collection is shipped. IDs, locations and all five metrics are
preserved. Roof associations are approximate and not parcel identifiers.
Historical sale prices are not current valuations. Source DGFiP / Etalab,
DVF 2020; acquisition and transformation lineage still need release review.
All other collection batches are preserved from the France-temperature sample.


## Expanded local scene, cross-collection v2

At Rémi's request, the scene now has a 500 m radius (1 km diameter).
`indexus-sample-3d-cross-collection-v2.tar.xz`: 114,424,940 bytes, SHA256
`de53f1c38734787c159df147070c10fddf0d2ce9d25e9088943a5f5d15e1f20d`.
All 14 batches pass prep verification; 308,551 records in total. The same
prepared DVF 2020 source supplies 19 house sales inside the scene disc. IDs,
locations and metrics are unchanged. The 7 weather batches are copied unchanged
from the verified France-temperature sample. Expanded buildings, terrain,
vegetation and imagery come from the same prepared local exports as v1.
No new data acquisition is made. Earlier v1 figures above remain historical.

### Reproduce the current local lineage check

From the core checkout:

```sh
python3 scripts/local/inspect-sample-provenance.py \
  --root /path/to/indexus \
  --sample /path/to/scene-cross-collection-1km \
  --weather-sample /path/to/scene-weather-france-v1 \
  --dvf-source /path/to/prepared/dvf-2020.ndjson \
  --out /path/to/current-provenance-review.json
```

The 18 November run checked all 46 files in the archive against the prepared
directory, including data parts and zone files, rather than only comparing
manifests. All seven AROME batch directories are byte-identical to the earlier
France-temperature sample. The 19 DVF records exactly reproduce the canonical
500 m-disc crop from the prepared source hash recorded above, with unchanged
IDs, locations, metrics and order; the crop hash also matches its prep manifest.
The same tool still validates the earlier 13-collection sample. A negative
fixture that changes a data part while leaving manifests unchanged is rejected
without writing a report.

These checks close local packaging and crop-lineage gaps. They do not recover
missing upstream acquisition dates, establish the original DVF export's full
lineage, verify town-label/land-mask rights, or approve public distribution.
No data, image or software licence was changed by this inspection.

### Town labels and land mask: bounded source review, 18 November

The current `arome_cities.py` defines 120 labels inline and reads 400 more from
`arome_communes.csv`. All 520 names and display priorities match the prepared
`TownsAROME0128v2.ndjson`; its SHA256
`17f277296a1505d427053d4507d682bea21192c00ad2e58a17d2a5fa9fdc12df`
matches the current batch manifest's input hash. The CSV's source comment names
the French API Découpage administratif for centres and populations. That is
evidence of an intended source, without a retained response URL, retrieval date
or source version. The 120 inline definitions do not inherit that claim.
The [official API documentation](https://geo.api.gouv.fr/decoupage-administratif/communes)
was checked on 18 November; it does not establish the provenance or terms of
our historical CSV by itself. No replacement data was fetched.

The Python prep encoder reproduces all 520 stored locations from these
coordinates. A separate re-encoding with the public JS SDK `Collection`/`Space`
facade matches 510 exactly, with 10 differences at 16-character depth. In those
cases the input point lies at most approximately 1.42e-14 degrees outside the
stored cell's JS-decoded bounds. Python's normalized-grid calculation and JS's
repeated midpoint bisection are different numerical paths. This is an explicit
address-conformity gap, not a licence finding or an approved key migration.
Keep the stored addresses unchanged pending a separate compatibility review.

`public/france.geojson` has SHA256
`45804839ae477855bfc8b45a5bbc03e30d79793db38389f5b4655ccd4b2ff374`.
It is byte-identical to its first tracked blob in experiences commit
`db0d07af48940f6856ecb1e3e02ef995aedc8909`. Its feature properties are empty;
neither an upstream URL nor a licence or acquisition date was recovered in this
bounded review. The local commit date is not an upstream acquisition date.
The AROME acquisition script uses this file for its land mask; a geographic
resemblance would not establish the producer or permission to redistribute.

The review report records source-file hashes and all ten coordinate examples:
`outputs/Indexus-weather-label-mask-provenance-2026-11-18.json` in the Codex chat
workspace. It establishes local relationships and narrows the remaining source
questions; town-label rights and the land mask's upstream provenance remain
open. No sample, runtime image, attribution wording or licence was changed.
