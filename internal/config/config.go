package config

import (
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Config holds all runtime configuration, loaded from environment variables.
type Config struct {
	// Role selects which halves of the binary this instance runs: the three
	// worker loops, the public HTTP API, or both. It is per box (.env.host).
	Role Role

	// InstanceID attributes log lines and claims (claimed_by) to a box. It
	// does not have to be unique: the claim owner is InstanceID plus a random
	// per-boot nonce, so two boxes sharing a name — or every box falling back
	// to "unknown" — still cannot pass each other's claim guards.
	//
	// It never contains '/' or a control character (Load replaces them with
	// '-'): '/' separates it from the nonce in claimed_by, and the runbook
	// splits on it to group claims by box.
	InstanceID string

	// Database
	DatabaseURL string

	// DBMaxConns is this instance's pgx pool size. Every instance shares the
	// one PostgreSQL, so the fleet's pools have to add up to less than its
	// max_connections. Load applies the same default and floor as db.Connect
	// (<= 0 → 10, below 4 → 4), so the boot log shows the pool that was
	// really opened rather than the number somebody typed.
	DBMaxConns int

	// CronSchedule — standard cron expression (robfig/cron: 5 fields, or a
	// descriptor such as @every 12h). It no longer triggers anything: the
	// worker loops run continuously, and the schedule only defines the budget
	// windows. Each activation starts a new window with a fresh
	// APIBudgetPerCycle. Every instance derives the window from its own copy,
	// so the value must be identical on every box (.env.fleet). It is
	// validated by internal/budget at startup, not here.
	CronSchedule string

	// OpenWebNinja (Zillow) API
	ZillowBaseURL string
	ZillowAPIKey  string

	// LocationIQ — static property maps generated on demand by the detail
	// endpoint. Empty disables maps entirely.
	LocationIQAPIKey string

	// ImagesEnabled controls whether listing images are downloaded and uploaded
	// to Bunny CDN. When false, the source Zillow image URLs are stored directly
	// and Bunny config is not required (useful for local/dev runs).
	ImagesEnabled bool

	// SkipExisting, when true, leaves already-stored listings (by zpid)
	// untouched — no re-upsert and no re-render. When false, existing listings
	// are updated each cycle (refreshing price, photos, etc.).
	SkipExisting bool

	// DetailsPerCycle is the details share of APIBudgetPerCycle: how many
	// one-time details-API enrichment calls the whole fleet may make per
	// budget window (protects API quota). <= 0 disables enrichment. Like
	// APIBudgetPerCycle it must be identical on every box.
	DetailsPerCycle int

	// APIBudgetPerCycle is the FLEET-WIDE cap on OpenWebNinja requests (search
	// pages + details calls, retries included) per budget window — see
	// CronSchedule — so a month of windows fits the API plan's quota. It is
	// not a per-instance allowance: every instance draws from one ledger in
	// PostgreSQL, and adding a box adds no budget. Search gets
	// APIBudgetPerCycle - DetailsPerCycle; details keeps its own cap. The
	// ledger enforces whatever limit the caller passes, so the effective
	// fleet limit is the largest value any instance is running with: it must
	// be identical on every box. (The name predates the fleet, when a window
	// was one collection cycle.)
	APIBudgetPerCycle int

	// QueueHighWater is the discovery loop's backpressure: while at least
	// this many listings are claimable in listing_queue, no further ZIP is
	// searched, so paid search results do not pile up faster than the fleet
	// can render them. <= 0 disables backpressure.
	QueueHighWater int

	// Bunny CDN storage
	BunnyStorageZone string
	BunnyAPIKey      string
	BunnyStorageHost string // e.g. storage.bunnycdn.com or la.storage.bunnycdn.com
	BunnyCDNBaseURL  string // public pull-zone base, e.g. https://dwellings.b-cdn.net

	// Search criteria shared across all searched locations (home status, price,
	// bedrooms, max results). The per-search Location is filled in from the
	// ZIP rotation by the scheduler.
	Search SearchCriteria

	// Video rendering
	Video VideoConfig

	// Linear channels — 24/7 HLS streams assembled from the listing videos.
	Linear LinearConfig

	// Viewer tracking and the channel resolver.
	Viewer ViewerConfig

	// Ads — VAST ad tags handed to the Roku app.
	Ads AdsConfig

	// PublicBaseURL is this server's public origin (e.g. https://api.dwellings.tv)
	// for absolute URLs in the Roku feed. Empty disables the feed's live entry.
	PublicBaseURL string

	// HTTPPort is the port for the Roku feed + health HTTP server.
	HTTPPort string

	// Concurrency limits for the collection cycle.
	Concurrency ConcurrencyConfig

	// HTTP client timeout for outbound API calls (Zillow, image downloads).
	HTTPTimeout time.Duration

	// BunnyTimeout is the upload timeout for Bunny CDN. Videos are large, so this
	// is much longer than the general HTTP timeout.
	BunnyTimeout time.Duration
}

// ConcurrencyConfig bounds the parallelism of the collection cycle.
type ConcurrencyConfig struct {
	// Listings is how many listings are processed concurrently. Each listing
	// runs a CPU-heavy ffmpeg render, so this defaults to the CPU count.
	Listings int
	// Images is how many photos are downloaded/uploaded concurrently within a
	// single listing (I/O-bound).
	Images int
}

// VideoConfig controls listing-video rendering.
type VideoConfig struct {
	Enabled         bool
	SecondsPerPhoto int
	MusicDir        string
	FontPath        string
	// FPS is the frame rate of the rendered video. The listings are still
	// photographs under a static overlay, so frames beyond the first of each
	// photo are duplicates and a lower rate is nearly free throughput:
	// measured 97 s at 30 fps against 63 s at 15 fps for the same listing,
	// costing 9% file size. 0 keeps the 30 fps everything was rendered at.
	FPS int
	// Threads caps the pools one render may open (decoders, filter graph,
	// x264). ffmpeg sizes them from the core count it sees, which on a box
	// running one render per core means every render asks for the whole
	// machine. Defaults to vCPUs / LISTING_CONCURRENCY, at least 1.
	Threads int
}

// LinearConfig controls the linear channels.
type LinearConfig struct {
	Enabled         bool
	LineupHours     int // max content per lineup version
	MinScopeClips   int // a scope with fewer clips falls back to its parent area
	EPGHorizonHours int // how far ahead the EPG materialises the schedule
	// LiveThumbnailURL is the poster of the Roku liveFeeds entry. Empty
	// borrows the first ready listing's image; with neither, the entry is
	// omitted (Roku requires a thumbnail).
	LiveThumbnailURL string
}

// ViewerConfig controls pseudonymous viewer tracking on the linear channels
// (see docs/superpowers/specs/2026-08-26-viewer-tracking-design.md).
type ViewerConfig struct {
	Enabled bool
	// Salt is mixed into every viewer hash. Required when tracking is on and
	// the role serves the API (a worker hashes nobody and loads without it);
	// changing it re-identifies everyone.
	Salt string
	// RotateDaily also mixes in the UTC date, so ids cannot be linked
	// across days (and "last watched" only survives the day).
	RotateDaily   bool
	RetentionDays int
	// GeoIPDBPath is a MaxMind GeoLite2-City .mmdb; empty disables the IP
	// geo default in /channels/resolve.
	GeoIPDBPath string
}

// AdsConfig holds the VAST ad tag URLs from the ad server. The backend never
// calls them: /api/v1/properties and /channels/resolve return them as
// pre_roll_ad / mid_roll_ad, and the Roku app plays
// them through RAF, substituting the device macros (ROKU_ADS_TRACKING_ID…)
// itself. Empty means no ads for that slot.
type AdsConfig struct {
	PrerollURL string
	MidrollURL string
}

// SearchCriteria defines what properties the scheduler discovers each cycle.
type SearchCriteria struct {
	Location    string // e.g. "Punta Gorda, FL"
	HomeStatus  string // FOR_SALE | FOR_RENT | RECENTLY_SOLD (default FOR_SALE)
	MinPrice    int
	MaxPrice    int
	MinBedrooms int
	// MaxResults truncates one ZIP's search; 0 (the default) is unlimited.
	// The default used to be 50, but a ZIP is stamped searched whether or not
	// its results were cut short, so a worker provisioned without the
	// override would silently drop the rest of every dense ZIP until the
	// rotation came round again. Spend is bounded by the fleet budget ledger
	// instead.
	MaxResults int
	MaxPages   int // per-search page cap set by the scheduler; 0 = client hard cap
}

// Role is what one instance of the binary does. The fleet is about ten
// workers around a single API box, all running the same image.
type Role string

const (
	// RoleAll runs the worker loops and the public API in one process: the
	// single-instance deployment, and the default so that an existing box
	// needs no new configuration.
	RoleAll Role = "all"
	// RoleAPI serves HTTP only.
	RoleAPI Role = "api"
	// RoleWorker runs the worker loops and serves nothing but /healthz.
	RoleWorker Role = "worker"
)

// RunsWorkers reports whether this role runs the discovery, media and details
// loops. The zero Role, like any value Load would have rejected, runs nothing.
func (r Role) RunsWorkers() bool { return r == RoleAll || r == RoleWorker }

// ServesAPI reports whether this role mounts the public HTTP routes. The zero
// Role, like any value Load would have rejected, serves nothing.
func (r Role) ServesAPI() bool { return r == RoleAll || r == RoleAPI }

// parseRole reads ROLE. Blank means unset; anything else has to be one of the
// three roles. There is deliberately no fallback: ROLE=workers quietly
// becoming "all" would put a second scheduler-plus-public-API on a box that
// was provisioned as a worker.
func parseRole(raw string) (Role, error) {
	switch r := Role(strings.ToLower(strings.TrimSpace(raw))); r {
	case "":
		return RoleAll, nil
	case RoleAll, RoleAPI, RoleWorker:
		return r, nil
	default:
		return "", fmt.Errorf("ROLE must be one of %s, %s, %s, got %q", RoleAll, RoleAPI, RoleWorker, raw)
	}
}

// Pool sizing, mirroring internal/db (config imports no internal package).
const (
	defaultDBMaxConns = 10
	minDBMaxConns     = 4
)

// unknownInstanceID stands in when neither INSTANCE_ID nor the hostname is
// usable; see Config.InstanceID for why that is safe.
const unknownInstanceID = "unknown"

// osHostname is os.Hostname, replaceable in tests.
var osHostname = os.Hostname

// instanceID resolves INSTANCE_ID, defaulting to the hostname. It never fails
// and never returns "": attribution is a convenience, not worth refusing to
// boot over — which is also why an awkward value is cleaned rather than
// rejected.
func instanceID() string {
	if id := cleanInstanceID(getenv("INSTANCE_ID", "")); id != "" {
		return id
	}
	if host, err := osHostname(); err == nil {
		if host = cleanInstanceID(host); host != "" {
			return host
		}
	}
	return unknownInstanceID
}

// cleanInstanceID trims s and replaces '/' and control characters with '-'.
// The claim owner is InstanceID + "/" + nonce and the runbook reads the
// instance back with split_part(claimed_by, '/', 1): a slash inside the id
// would file "hel1/worker-01" and "hel1/worker-02" under one instance named
// "hel1". Control characters go because the id is printed in every log line.
// Replacing rather than dropping keeps "a/b" and "ab" apart.
func cleanInstanceID(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || unicode.IsControl(r) {
			return '-'
		}
		return r
	}, strings.TrimSpace(s))
}

// Load reads configuration from the environment, applying defaults and
// validating required values.
func Load() (*Config, error) {
	// First, because which variables are required depends on it.
	role, err := parseRole(os.Getenv("ROLE"))
	if err != nil {
		return nil, err
	}

	c := &Config{
		Role:              role,
		InstanceID:        instanceID(),
		DatabaseURL:       getenv("DATABASE_URL", ""),
		DBMaxConns:        getenvInt("DB_MAX_CONNS", defaultDBMaxConns),
		CronSchedule:      getenv("CRON_SCHEDULE", "0 */12 * * *"), // every 12 hours
		ZillowBaseURL:     getenv("ZILLOW_BASE_URL", "https://api.openwebninja.com/realtime-zillow-data"),
		ZillowAPIKey:      getenv("ZILLOW_API_KEY", ""),
		LocationIQAPIKey:  getenv("LOCATIONIQ_API_KEY", ""),
		ImagesEnabled:     getenvBool("IMAGES_ENABLED", true),
		SkipExisting:      getenvBool("SKIP_EXISTING", true),
		DetailsPerCycle:   getenvInt("DETAILS_PER_CYCLE", 50),
		APIBudgetPerCycle: getenvInt("API_BUDGET_PER_CYCLE", 150),
		QueueHighWater:    getenvInt("QUEUE_HIGH_WATER", 2000),
		BunnyStorageZone:  getenv("BUNNY_STORAGE_ZONE", ""),
		BunnyAPIKey:       getenv("BUNNY_API_KEY", ""),
		BunnyStorageHost:  getenv("BUNNY_STORAGE_HOST", "storage.bunnycdn.com"),
		BunnyCDNBaseURL:   strings.TrimRight(getenv("BUNNY_CDN_BASE_URL", ""), "/"),
		Search: SearchCriteria{
			HomeStatus:  getenv("SEARCH_HOME_STATUS", "FOR_SALE"),
			MinPrice:    getenvInt("SEARCH_MIN_PRICE", 0),
			MaxPrice:    getenvInt("SEARCH_MAX_PRICE", 0),
			MinBedrooms: getenvInt("SEARCH_MIN_BEDROOMS", 0),
			MaxResults:  getenvInt("SEARCH_MAX_RESULTS", 0),
		},
		Video: VideoConfig{
			Enabled:         getenvBool("VIDEO_ENABLED", true),
			SecondsPerPhoto: getenvInt("VIDEO_SECONDS_PER_PHOTO", 4),
			MusicDir:        getenv("MUSIC_DIR", "assets/music"),
			FontPath:        getenv("VIDEO_FONT_PATH", "/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf"),
			Threads:         getenvInt("VIDEO_FFMPEG_THREADS", 0),
			FPS:             getenvInt("VIDEO_FPS", 0),
		},
		Linear: LinearConfig{
			Enabled:          getenvBool("LINEAR_ENABLED", true),
			LineupHours:      getenvInt("LINEAR_LINEUP_HOURS", 6),
			MinScopeClips:    getenvInt("LINEAR_MIN_SCOPE_CLIPS", 20),
			EPGHorizonHours:  getenvInt("LINEAR_EPG_HORIZON_HOURS", 24),
			LiveThumbnailURL: strings.TrimSpace(getenv("LINEAR_LIVE_THUMBNAIL_URL", "")),
		},
		Viewer: ViewerConfig{
			Enabled:       getenvBool("VIEWER_TRACKING_ENABLED", true),
			Salt:          strings.TrimSpace(getenv("VIEWER_SALT", "")),
			RotateDaily:   getenvBool("VIEWER_SALT_ROTATE_DAILY", false),
			RetentionDays: getenvInt("VIEWER_RETENTION_DAYS", 30),
			GeoIPDBPath:   strings.TrimSpace(getenv("GEOIP_DB_PATH", "")),
		},
		Ads: AdsConfig{
			PrerollURL: strings.TrimSpace(getenv("AD_PREROLL_URL", "")),
			MidrollURL: strings.TrimSpace(getenv("AD_MIDROLL_URL", "")),
		},
		PublicBaseURL: strings.TrimRight(getenv("PUBLIC_BASE_URL", ""), "/"),
		HTTPPort:      getenv("HTTP_PORT", "8080"),
		Concurrency: ConcurrencyConfig{
			Listings: getenvInt("LISTING_CONCURRENCY", runtime.NumCPU()),
			Images:   getenvInt("IMAGE_CONCURRENCY", 8),
		},
		HTTPTimeout:  time.Duration(getenvInt("HTTP_TIMEOUT_SECONDS", 30)) * time.Second,
		BunnyTimeout: time.Duration(getenvInt("BUNNY_TIMEOUT_SECONDS", 300)) * time.Second,
	}
	if c.Concurrency.Listings < 1 {
		c.Concurrency.Listings = 1
	}
	// Share the machine between the renders that run on it. Unset, every
	// render would size its pools for every core, so N concurrent renders ask
	// for N times the machine and the box thrashes.
	if c.Video.Threads <= 0 {
		c.Video.Threads = max(1, runtime.NumCPU()/c.Concurrency.Listings)
	}
	if c.Concurrency.Images < 1 {
		c.Concurrency.Images = 1
	}
	switch {
	case c.DBMaxConns <= 0:
		c.DBMaxConns = defaultDBMaxConns
	case c.DBMaxConns < minDBMaxConns:
		c.DBMaxConns = minDBMaxConns
	}

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.ZillowAPIKey == "" {
		missing = append(missing, "ZILLOW_API_KEY")
	}
	// Bunny is required whenever something uploads to it: listing images
	// (ImagesEnabled) or property maps (LocationIQAPIKey set). Without this,
	// maps could be enabled with images disabled and every admitted
	// generation would geocode and fetch a static map only to fail at the
	// upload step — burning LocationIQ quota with no possible success.
	if c.ImagesEnabled || c.LocationIQAPIKey != "" {
		if c.BunnyStorageZone == "" {
			missing = append(missing, "BUNNY_STORAGE_ZONE")
		}
		if c.BunnyAPIKey == "" {
			missing = append(missing, "BUNNY_API_KEY")
		}
		if c.BunnyCDNBaseURL == "" {
			missing = append(missing, "BUNNY_CDN_BASE_URL")
		}
	}
	// Only a process that serves the API hashes viewers. A worker still reads
	// LINEAR_ENABLED (it segments HLS), so without the role check every worker
	// box would need a copy of the secret it never uses.
	if c.Role.ServesAPI() && c.Linear.Enabled && c.Viewer.Enabled && c.Viewer.Salt == "" {
		missing = append(missing, "VIEWER_SALT (or VIEWER_TRACKING_ENABLED=false)")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	for key, v := range map[string]string{"AD_PREROLL_URL": c.Ads.PrerollURL, "AD_MIDROLL_URL": c.Ads.MidrollURL} {
		if v != "" && !isHTTPURL(v) {
			return nil, fmt.Errorf("%s must be an absolute http(s) URL, got %q", key, v)
		}
	}
	if c.Viewer.RetentionDays < 1 {
		c.Viewer.RetentionDays = 1
	}

	return c, nil
}

// isHTTPURL reports whether s is an absolute http(s) URL with a host. Ad tags
// carry macros like [CACHEBUSTER] in the query, which url.Parse tolerates.
func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// getenvBool and getenvInt fall back to def when the value does not parse.
// They trim first because strconv does not: ROLE and INSTANCE_ID are trimmed,
// so a CRLF env file (or a quoted "0 ") boots fine, and without the trim
// every number and flag in it would silently be its default instead —
// QUEUE_HIGH_WATER="0\r" leaving backpressure on, API_BUDGET_PER_CYCLE="0\r"
// spending 150 requests a window.
func getenvBool(key string, def bool) bool {
	if b, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key))); err == nil {
		return b
	}
	return def
}

func getenvInt(key string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return n
	}
	return def
}
