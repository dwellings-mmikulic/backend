package config

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration, loaded from environment variables.
type Config struct {
	// Database
	DatabaseURL string

	// Scheduler — standard cron expression (robfig/cron, 5 fields).
	CronSchedule string

	// OpenWebNinja (Zillow) API
	ZillowBaseURL string
	ZillowAPIKey  string

	// ImagesEnabled controls whether listing images are downloaded and uploaded
	// to Bunny CDN. When false, the source Zillow image URLs are stored directly
	// and Bunny config is not required (useful for local/dev runs).
	ImagesEnabled bool

	// SkipExisting, when true, leaves already-stored listings (by zpid)
	// untouched — no re-upsert and no re-render. When false, existing listings
	// are updated each cycle (refreshing price, photos, etc.).
	SkipExisting bool

	// DetailsPerCycle caps how many properties get a one-time details-API
	// enrichment call per cycle (protects API quota). <= 0 disables enrichment.
	DetailsPerCycle int

	// Bunny CDN storage
	BunnyStorageZone string
	BunnyAPIKey      string
	BunnyStorageHost string // e.g. storage.bunnycdn.com or la.storage.bunnycdn.com
	BunnyCDNBaseURL  string // public pull-zone base, e.g. https://dwellings.b-cdn.net

	// Search criteria shared across all searched locations (home status, price,
	// bedrooms, max results). The per-search Location is filled in from
	// SearchLocations by the scheduler.
	Search SearchCriteria

	// SearchLocations is the set of locations (ZIP codes) searched each cycle,
	// parsed from the comma-separated SEARCH_LOCATION env var.
	SearchLocations []string

	// Video rendering
	Video VideoConfig

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
}

// SearchCriteria defines what properties the scheduler discovers each cycle.
type SearchCriteria struct {
	Location    string // e.g. "Punta Gorda, FL"
	HomeStatus  string // FOR_SALE | FOR_RENT | RECENTLY_SOLD (default FOR_SALE)
	MinPrice    int
	MaxPrice    int
	MinBedrooms int
	MaxResults  int
}

// Load reads configuration from the environment, applying defaults and
// validating required values.
func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:      getenv("DATABASE_URL", ""),
		CronSchedule:     getenv("CRON_SCHEDULE", "0 */12 * * *"), // every 12 hours
		ZillowBaseURL:    getenv("ZILLOW_BASE_URL", "https://api.openwebninja.com/realtime-zillow-data"),
		ZillowAPIKey:     getenv("ZILLOW_API_KEY", ""),
		ImagesEnabled:    getenvBool("IMAGES_ENABLED", true),
		SkipExisting:     getenvBool("SKIP_EXISTING", true),
		DetailsPerCycle:  getenvInt("DETAILS_PER_CYCLE", 50),
		BunnyStorageZone: getenv("BUNNY_STORAGE_ZONE", ""),
		BunnyAPIKey:      getenv("BUNNY_API_KEY", ""),
		BunnyStorageHost: getenv("BUNNY_STORAGE_HOST", "storage.bunnycdn.com"),
		BunnyCDNBaseURL:  strings.TrimRight(getenv("BUNNY_CDN_BASE_URL", ""), "/"),
		Search: SearchCriteria{
			HomeStatus:  getenv("SEARCH_HOME_STATUS", "FOR_SALE"),
			MinPrice:    getenvInt("SEARCH_MIN_PRICE", 0),
			MaxPrice:    getenvInt("SEARCH_MAX_PRICE", 0),
			MinBedrooms: getenvInt("SEARCH_MIN_BEDROOMS", 0),
			MaxResults:  getenvInt("SEARCH_MAX_RESULTS", 50),
		},
		SearchLocations: parseLocations(getenv("SEARCH_LOCATION", "")),
		Video: VideoConfig{
			Enabled:         getenvBool("VIDEO_ENABLED", true),
			SecondsPerPhoto: getenvInt("VIDEO_SECONDS_PER_PHOTO", 4),
			MusicDir:        getenv("MUSIC_DIR", "assets/music"),
			FontPath:        getenv("VIDEO_FONT_PATH", "/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf"),
		},
		HTTPPort: getenv("HTTP_PORT", "8080"),
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
	if c.Concurrency.Images < 1 {
		c.Concurrency.Images = 1
	}

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.ZillowAPIKey == "" {
		missing = append(missing, "ZILLOW_API_KEY")
	}
	if c.ImagesEnabled {
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
	if len(c.SearchLocations) == 0 {
		missing = append(missing, "SEARCH_LOCATION")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	return c, nil
}

// parseLocations splits a comma-separated location list (e.g. "33950,33948")
// into trimmed, non-empty entries, preserving order.
func parseLocations(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if loc := strings.TrimSpace(part); loc != "" {
			out = append(out, loc)
		}
	}
	return out
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getenvBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func getenvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
