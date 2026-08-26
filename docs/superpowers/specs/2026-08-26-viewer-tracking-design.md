# Viewer tracking + channel resolve (slice 1) — design

Approved in chat 2026-08-26. Builds on the linear channels
(`2026-08-25-linear-channels-design.md`).

## Goal

Know who is watching which channel without storing raw IPs, and give the
Roku app a one-call way to pick the right channel for a viewer
(last watched → IP geo → national).

## Identity (`internal/viewer`)

`Hasher.ID(r)` → 16-byte id = first 16 bytes of
`sha256(salt || "\x00" || kind || "\x00" || value)`.
`kind/value` is `sid`/`?sid=` when present (Roku RIDA later), else
`ip`/client IP (`X-Real-IP`, then first `X-Forwarded-For`, then RemoteAddr).
Salt = `VIEWER_SALT`; with `VIEWER_SALT_ROTATE_DAILY=true` the UTC date is
appended so ids are unlinkable across days. Raw IPs and sids are never
stored or logged.

## Heartbeats

Every `GET /channels/live.m3u8` is a heartbeat: the handler records
`(viewer, channel_key, minute)` in an in-memory set that `Recorder.Run`
flushes to Postgres every 30 s (`INSERT … ON CONFLICT DO NOTHING`) and
upserts `viewer_last_scope`. Rows older than `VIEWER_RETENTION_DAYS` (30)
are purged hourly by the same loop.

```sql
viewer_heartbeats(viewer_hash BYTEA, channel_key TEXT, minute TIMESTAMPTZ,
                  PRIMARY KEY (viewer_hash, channel_key, minute))
INDEX (channel_key, minute)
viewer_last_scope(viewer_hash BYTEA PRIMARY KEY, channel_key TEXT, seen_at TIMESTAMPTZ)
```

nginx stops caching `live.m3u8` (master + EPG stay cached) so every player
poll reaches the app. The playlist is computed from memory; one request per
viewer per few seconds is negligible.

## Geo (`internal/geo`)

MaxMind GeoLite2-City `.mmdb` at `GEOIP_DB_PATH`, read with
`oschwald/geoip2-golang`. `Locate(ip)` → postal code, city, state (US only).
Missing/unreadable DB → geo disabled with one warning; resolve still works.
`geoipupdate` on the web box refreshes the file weekly.

## Endpoints (on `linear.Handler`)

- `GET /channels/resolve` — `{"scope","name","master","epg","source"}`.
  Candidates in order: last watched (if seen within 30 d) → geo ZIP →
  national. Each candidate goes through the existing scope fallback
  (`resolveScope`), so the answer always has content. `source` is
  `last_watched | geo | default`. `Cache-Control: no-store`.
- `GET /channels/stats?scope=…` — `{"scope","concurrent","unique_24h","unique_7d"}`.
  concurrent = distinct viewers in the last 2 minutes. Cached 60 s by nginx.

## Config

`VIEWER_SALT` (required when tracking is on; startup error if empty),
`VIEWER_SALT_ROTATE_DAILY` (false), `VIEWER_RETENTION_DAYS` (30),
`GEOIP_DB_PATH` (empty = geo off). All under `LINEAR_ENABLED`.

## Out of scope

Roku app changes, ad integration, dashboards, auth on stats.
