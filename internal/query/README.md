<!--
  README.md
  Query sampling contract.

  Created: 2026-08-31
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Query sampling

Sampled raw queries select deterministic materialized buckets independently at
event and session grain. Membership is allocated from each site/day fact
ordinal, not derived from caller-controlled row ids, so signed imports, sparse
ids, restores, and ordinary deletes do not create low-bit bias. The indexed
membership list drives primary-key fact fetches and bounds row work by selected
buckets. Additive event/session totals are expanded by the inverse rate. Rates,
averages, minima, maxima, and percentiles are computed directly from selected
rows; they are still estimates and can differ materially under skew. The API
does not claim a confidence interval.

`meta.sampling` names event-, session-, and mixed-grain metrics, expected sampled
row work, sparse expected samples, and all-zero sampled results. One coherent
rate applies to primary and comparison periods.

Distinct visitors, event-grain distinct sessions, `has_done`, and other
operations requiring complete visitor or session event membership are refused
when sampling would be necessary. The API returns
`code: "sampling_requires_exact"`; the dashboard exposes one explicit exact
action and does not retry automatically. Set `exact: true`, remove the
unsupported operation, or use a summary/visit-grain representation. An exact
query remains available and is never labelled sampled.

Numeric-property coverage is derived from values actually observed in the
selected sample. Both observed and rate-expanded coverage are disclosed as
estimates; neither is presented as an exact full-range property count.
