# Platform feeds — design

Requested by Daniel 2026-10-27: Cineplex gets a "Monthly Sponsor — Explore
Nationwide Real Estate with Dwellings.TV" row carrying the FAST channel, on a
feed of its own that he can put ads on. Marko asked for it to be
platform-specific (not Cineplex-only) and path-based, not a query parameter.

## Routes

Every channel route is also served under `/channels/<platform>/`:

```
/channels/cineplex/master.m3u8   (live.m3u8 is referenced relatively, so a
/channels/cineplex/live.m3u8      player stays on the platform's path)
/channels/cineplex/epg.json
/channels/cineplex/epg.xml
/channels/cineplex/stats
```

Scope (`zip`, `city`+`state`, `state`), `sid` and `ip` work exactly as on
`/channels/…`. The content is the same channel; what differs is the URL (a
partner's ad stitcher or SSAI setup can be pointed at it alone) and the
audience accounting.

**Connecting address as the home (2026-11-06).** A platform's player is the
viewer's own box fetching `live.m3u8` directly (Cineplex's Roku app; every
request in the logs came from a residential address with no query string),
so the platform cannot append `ip=`. On `/channels/<platform>/live.m3u8`
without a public `ip=`, the connecting address is therefore the household:
created on first contact with the IP geo default (city → state → national,
as `/feed/` does), then served as that home's anchored feed. A home that
picked an area on the QR page keeps it. A non-public connecting address
(a stitcher or proxy on a private network) gets the channel as advertised
and no household. `/channels/live.m3u8` keeps its rule: never keyed on the
connection. nginx caches the platform live playlist per client address
(`deploy/nginx-dwellings.conf`), 2 s as before. Should a platform ever
front the feed with a stitcher, its address becomes every viewer's home;
the platform must then pass `ip=` instead. `/channels/resolve`, `/channels/beat` and the personal
feeds have no platform form.

Platforms are a closed list in code (`viewer.KnownPlatform`; today only
`cineplex`) because each name is a route and a stored value. An unknown or
differently-cased name is a 404.

## Tracking

`viewer.Client.Platform` carries the path segment into the heartbeat.
`viewer_heartbeats.platform` and `viewer_clients.platform` (TEXT NOT NULL
DEFAULT '', added with `ADD COLUMN IF NOT EXISTS`) store it; '' is our own
feed. The heartbeat primary key is unchanged, so a viewer polling two feeds
in the same minute is one row (the first one recorded). `/channels/<platform>/stats`
counts only that platform's viewers; `/channels/stats` still counts everyone.
`/admin/viewers` lists each client's platform.

Non-goals: per-platform VAST tags, per-platform lineups or branding.
