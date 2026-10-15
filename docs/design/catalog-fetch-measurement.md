# Catalog fetch measurement

`TestCatalogFetchAudit` builds a synthetic catalog of 4,600 works. Each work has
an edition, artwork, and a primary asset. The test sends HTTP requests through
the real router and SQLite database, follows cursors, parses every JSON page,
and checks the work count and response status. The timed region excludes fixture
creation. It includes server queries, JSON encoding, localhost transfer, and
client JSON decoding. It excludes network distance and desktop rendering.

Run the test with:

```sh
HEYARR_CATALOG_AUDIT=1 go test ./internal/api/resources -run '^TestCatalogFetchAudit$' -count=1 -v
```

The table compares five alternating runs of the baseline commit and this change
on the same machine. The host load was about 8 to 9 on 10 cores, so the runs
alternate to limit drift. Each range is the minimum and maximum of five runs.

| Fetch | Baseline median and range | Changed median and range | Requests |
| --- | --- | --- | --- |
| Full catalog, 200 rows per page | 1,138 ms, 964 to 1,253 ms | 387 ms, 308 to 438 ms | 23 in both |
| Recent 50 works | 89 ms, 81 to 119 ms | 9 ms, 3 to 11 ms | 1 in both |
| Three Listen content types | 5.2 ms, 1.4 to 7.7 ms | 5.7 ms, 2.3 to 6.2 ms | 3 in both |

SQLite used a temporary B-tree to sort the full and recent queries before the
new indexes. It now scans covering indexes for both orders. The filtered query
also uses a covering index with the `id` tie breaker. A separate CPU profile of
the timed fetches places most server CPU in `BrowseWorks` and SQLite's virtual
machine. JSON parsing in the test client accounts for more of the remainder.

The server now accepts 500 works per page. On this fixture, a full walk takes
10 requests instead of 23. The measured time difference between 200 and 500
rows per page is smaller than the run-to-run spread on localhost. The larger
page primarily saves round trips over a real network.

With `Accept-Encoding: gzip`, the 500-row full walk transfers 309,260 bytes
instead of 5,601,216 bytes on this fixture. The repeated synthetic titles and
artwork paths compress unusually well. This byte count does not predict the
compression ratio of another library. The timed gzip and plain runs overlap,
so this test does not establish a server latency gain from compression.

The desktop client reads recent pages for Home until it has enough cards.
Listen and Read use type-filtered requests. The test's three Listen requests returned three works in
3,686 bytes without gzip and 533 bytes with gzip. These are API path results,
not a desktop first-paint measurement. The client still needs a full catalog
when someone opens Watch or Manage, and older servers clamp its 500-row request
to 200 rows.
