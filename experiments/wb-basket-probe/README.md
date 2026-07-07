# wb-basket-probe

Measures the real WB CDN basket shard (`basket-NN.wbbasket.ru`) for high-vol
nmIDs, to calibrate `wbBasketNumber()` in `internal/scraper/wildberries.go`.

## Background

`wbImageURL` / card fetch pick a shard by `basket = wbBasketNumber(id)`, where
`vol = id/100000`. A product's `card.json` is served **200 only on its own
shard** and **404 on every other**. For `vol > 6437` the old code used the
formula `32 + (vol-6438)/312`, whose period (312) is far too small for modern
high vols and overshoots the basket number by up to +4 (e.g. vol 10961: formula
46, real 42). This probe pins the real boundaries so the lookup table can be
extended.

## Run

```bash
cd experiments/wb-basket-probe
./probe.sh all                       # harvest a default query set, then probe
# or, in two steps:
./probe.sh harvest капибара пряжа …  # -> ids.txt  (lines: "vol id")
./probe.sh probe ids.txt             # -> "vol id basket=N" per line
```

Requires `curl` and `python3`. Needs outbound HTTPS to `search.wb.ru` and
`basket-*.wbbasket.ru`.

## Why bash+curl (not Go)

The WB **search** endpoint sits behind the wbaas antibot, which fingerprints
Go's `net/http` (JA3/HTTP2) and answers **429**; curl's TLS fingerprint passes
for cold queries. The **basket CDN** has no antibot (plain 200/404), so any
client works there. Keep the query list small and cold — hammering search trips
a transient per-IP 429 (recovers after a few minutes).

## Results

See `results.txt` for the 2026-07-07 run: 36 measured `vol -> basket` points
across vol 6410..11625 (max live vol today), derived shard boundaries, and the
new tail period. Those boundaries were folded into `wbBasketNumber()` and its
unit test.
