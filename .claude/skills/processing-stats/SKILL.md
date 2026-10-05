---
name: processing-stats
description: Use when asked how production processing, collection, rendering, the worker fleet, or the listing queue is going, or whether the Dwellings pipeline is stalled, stuck, or keeping pace.
---

# Processing Stats

## Overview

One read-only script answers "how is processing going?" for the production
pipeline (ZIP discovery -> listing_queue -> render -> details). It runs psql on
web-01 against the private-net Postgres. Do not rediscover the schema or the
SSH path by hand.

## Run

```bash
bash .claude/skills/processing-stats/stats.sh            # snapshot
bash .claude/skills/processing-stats/stats.sh --errors   # + error text behind stuck queue items
```

Needs `~/.ssh/dwellings_tv` (override with `DWELLINGS_SSH_KEY`). Takes ~20 s.

## Reading the output

| Section | Healthy (measured 2026-10-05) | Worry when |
|---|---|---|
| `renders_1h` / `renders_24h` | ~1,500 / ~35k | < 1,000/h, or `last_render` more than a few minutes old |
| `new_24h` | tracks `renders_24h` (queue is kept short) | far above renders (backlog building) |
| `details_24h` | 8,000 (two 4,000 windows) | below 8,000 while `details_done` < total |
| per-day renders (7 days) | flat 33k–36k; today's row is partial, pro-rate by hour of day UTC | a full day drops sharply |
| `claimed_by` | 8 rows, worker-02..09, claims = that box's LISTING_CONCURRENCY (8/2/4/2/8/8/8/8) | a worker missing, or claims 0 |
| `listing_queue.depth` | a few thousand, `oldest_live` within hours | depth climbing for days, or `oldest_live` more than a day old |
| `stuck` (attempts >= 3) | 17 as of 2026-10-05 (all lot-size overflow, below) | > 30, or a new error class in `--errors` |
| `api_budget` search `spent` | well under 16,000/window | hitting 16,000 (backpressure broken) |
| `api_budget` details `spent` | exactly 4,000/window | far below 4,000 while details backlog exists |
| `zip_codes.searched` | slowly rising | `last_search` stale while queue is empty |
| `failed` videos | tens | hundreds |

Topology and the LISTING_CONCURRENCY per box live in the memory note
`multi-instance-workers`. Details has no backpressure; 4,000/window is a
deliberate throttle, so spending it out in minutes then idling is normal.

## Known stuck-item cause

`numeric field overflow` / `unable to encode N into binary format for int4` on
upsert: Zillow returned a lot size in the billions of sqft. Those rows retry 3
times and park. Fix is a clamp on ingest, not a queue purge.

## Common mistakes

- Sourcing `/opt/dwellings/.env` on the box: line 2 is a cron expression and
  bash tries to run it. The script greps `DATABASE_URL` instead.
- Querying a `listings` table: the table is `properties`.
- Reporting raw numbers without the comparison column above. The user wants
  "healthy or not", then the numbers.
