package config

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// setRequiredEnv sets the environment variables Load requires regardless of
// what this test is exercising, so each test only has to set what's relevant
// to it.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	clearFleetEnv(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("ZILLOW_API_KEY", "zillow-key")
	t.Setenv("SEARCH_LOCATION", "33950")
	t.Setenv("VIEWER_SALT", "test-salt")
}

// TestLoad_LocationIQWithoutImagesRequiresBunny covers finding 2: enabling
// maps (LOCATIONIQ_API_KEY set) with IMAGES_ENABLED=false and no Bunny
// config must fail loudly at startup, not silently burn LocationIQ quota on
// every admitted generation that then fails at the upload step.
func TestLoad_LocationIQWithoutImagesRequiresBunny(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("IMAGES_ENABLED", "false")
	t.Setenv("LOCATIONIQ_API_KEY", "pk.test")
	t.Setenv("BUNNY_STORAGE_ZONE", "")
	t.Setenv("BUNNY_API_KEY", "")
	t.Setenv("BUNNY_CDN_BASE_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("want error when LOCATIONIQ_API_KEY is set without Bunny config, got nil")
	}
	for _, want := range []string{"BUNNY_STORAGE_ZONE", "BUNNY_API_KEY", "BUNNY_CDN_BASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name missing variable %q", err, want)
		}
	}
}

// TestLoad_LocationIQWithoutImagesSucceedsWithBunny is the positive
// counterpart: the same LOCATIONIQ_API_KEY + IMAGES_ENABLED=false setup
// succeeds once Bunny is configured.
func TestLoad_LocationIQWithoutImagesSucceedsWithBunny(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("IMAGES_ENABLED", "false")
	t.Setenv("LOCATIONIQ_API_KEY", "pk.test")
	t.Setenv("BUNNY_STORAGE_ZONE", "zone")
	t.Setenv("BUNNY_API_KEY", "key")
	t.Setenv("BUNNY_CDN_BASE_URL", "https://cdn.example.com")

	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// minimalEnv sets just enough for Load to succeed, with images disabled so
// Bunny vars are not required.
func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ZILLOW_API_KEY", "test-key")
	t.Setenv("IMAGES_ENABLED", "false")
	t.Setenv("LOCATIONIQ_API_KEY", "")
	t.Setenv("SEARCH_LOCATION", "")
	t.Setenv("API_BUDGET_PER_CYCLE", "")
	t.Setenv("VIEWER_SALT", "test-salt")
	clearFleetEnv(t)
}

// clearFleetEnv blanks (= unsets, see getenv) the variables the fleet
// settings read, so that a ROLE or INSTANCE_ID exported in the developer's
// shell cannot leak into a test. ROLE matters most: an unknown value fails
// every Load, and ROLE=worker changes which variables are required.
func clearFleetEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"ROLE", "INSTANCE_ID", "DB_MAX_CONNS", "QUEUE_HIGH_WATER", "SEARCH_MAX_RESULTS",
		"LINEAR_ENABLED", "VIEWER_TRACKING_ENABLED",
	} {
		t.Setenv(key, "")
	}
}

func TestLoad_APIBudgetDefault(t *testing.T) {
	minimalEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIBudgetPerCycle != 150 {
		t.Errorf("APIBudgetPerCycle = %d, want 150", cfg.APIBudgetPerCycle)
	}
}

func TestLoad_APIBudgetFromEnv(t *testing.T) {
	minimalEnv(t)
	t.Setenv("API_BUDGET_PER_CYCLE", "300")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIBudgetPerCycle != 300 {
		t.Errorf("APIBudgetPerCycle = %d, want 300", cfg.APIBudgetPerCycle)
	}
}

func TestLoad_SearchLocationNotRequired(t *testing.T) {
	minimalEnv(t)
	if _, err := Load(); err != nil {
		t.Fatalf("Load without SEARCH_LOCATION: %v", err)
	}
}

func TestLoad_LinearDefaults(t *testing.T) {
	clearFleetEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("ZILLOW_API_KEY", "k")
	t.Setenv("IMAGES_ENABLED", "false")
	t.Setenv("PUBLIC_BASE_URL", "https://api.example.com/")
	t.Setenv("VIEWER_SALT", "s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Linear.Enabled || cfg.Linear.LineupHours != 6 || cfg.Linear.MinScopeClips != 20 || cfg.Linear.EPGHorizonHours != 24 {
		t.Errorf("linear defaults = %+v", cfg.Linear)
	}
	if cfg.PublicBaseURL != "https://api.example.com" {
		t.Errorf("PublicBaseURL = %q (trailing slash must be trimmed)", cfg.PublicBaseURL)
	}
}

func TestLoad_ViewerSaltRequiredWithTracking(t *testing.T) {
	minimalEnv(t)
	t.Setenv("VIEWER_SALT", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "VIEWER_SALT") {
		t.Errorf("tracking on without a salt must fail at startup, got %v", err)
	}
	t.Setenv("VIEWER_TRACKING_ENABLED", "false")
	if _, err := Load(); err != nil {
		t.Errorf("tracking off needs no salt: %v", err)
	}
	t.Setenv("VIEWER_TRACKING_ENABLED", "")
	t.Setenv("LINEAR_ENABLED", "false")
	if _, err := Load(); err != nil {
		t.Errorf("no linear channels, no tracking, no salt needed: %v", err)
	}
}

func TestLoad_ViewerDefaults(t *testing.T) {
	minimalEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Viewer
	if !v.Enabled || v.Salt != "test-salt" || v.RotateDaily || v.RetentionDays != 30 || v.GeoIPDBPath != "" {
		t.Errorf("viewer defaults = %+v", v)
	}
	t.Setenv("VIEWER_SALT_ROTATE_DAILY", "true")
	t.Setenv("VIEWER_RETENTION_DAYS", "0")
	t.Setenv("GEOIP_DB_PATH", " /geoip/x.mmdb ")
	cfg, _ = Load()
	if !cfg.Viewer.RotateDaily || cfg.Viewer.RetentionDays != 1 || cfg.Viewer.GeoIPDBPath != "/geoip/x.mmdb" {
		t.Errorf("viewer env = %+v", cfg.Viewer)
	}
}

func TestLoad_AdTags(t *testing.T) {
	minimalEnv(t)
	const pre = "https://ads.example.com/vast?pod=pre&cb=[CACHEBUSTER]&did=ROKU_ADS_TRACKING_ID"
	t.Setenv("AD_PREROLL_URL", " "+pre+" ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ads.PrerollURL != pre || cfg.Ads.MidrollURL != "" {
		t.Errorf("ads = %+v", cfg.Ads)
	}
}

func TestLoad_AdTagMustBeAbsoluteHTTPURL(t *testing.T) {
	for _, bad := range []string{"ads.example.com/vast", "ftp://ads.example.com/vast", "https://"} {
		t.Run(bad, func(t *testing.T) {
			minimalEnv(t)
			t.Setenv("AD_MIDROLL_URL", bad)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "AD_MIDROLL_URL") {
				t.Errorf("Load() error = %v, want one naming AD_MIDROLL_URL", err)
			}
		})
	}
}

// TestLoad_RoleDefault pins the "no new required configuration" promise: a
// box that never heard of ROLE keeps being the scheduler + API process.
func TestLoad_RoleDefault(t *testing.T) {
	minimalEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Role != RoleAll {
		t.Errorf("Role = %q, want %q", cfg.Role, RoleAll)
	}
}

func TestLoad_RoleFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want Role
	}{
		{"all", RoleAll},
		{"api", RoleAPI},
		{"worker", RoleWorker},
		{"WORKER", RoleWorker},
		{"Api", RoleAPI},
		{"  worker  ", RoleWorker},
		{"\tALL\n", RoleAll},
		// Whitespace only is "unset", like every other blank variable here.
		{"   ", RoleAll},
	} {
		t.Run(tc.env, func(t *testing.T) {
			minimalEnv(t)
			t.Setenv("ROLE", tc.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Role != tc.want {
				t.Errorf("ROLE=%q: Role = %q, want %q", tc.env, cfg.Role, tc.want)
			}
		})
	}
}

// TestLoad_RoleInvalid: a typo must stop the boot. Falling back to "all"
// would quietly turn a worker box into a second public API + scheduler.
func TestLoad_RoleInvalid(t *testing.T) {
	for _, bad := range []string{"workers", "scheduler", "all,api", "a p i", "0"} {
		t.Run(bad, func(t *testing.T) {
			minimalEnv(t)
			t.Setenv("ROLE", bad)
			cfg, err := Load()
			if err == nil {
				t.Fatalf("ROLE=%q: Load succeeded with Role=%q, want an error", bad, cfg.Role)
			}
			if cfg != nil {
				t.Errorf("ROLE=%q: Load returned a config alongside the error", bad)
			}
			for _, want := range []string{"ROLE", `"` + bad + `"`, "all", "api", "worker"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestLoad_RoleInvalidReportedFirst: which variables are required depends on
// the role, so a bad role is reported instead of a list computed from a guess.
func TestLoad_RoleInvalidReportedFirst(t *testing.T) {
	minimalEnv(t)
	t.Setenv("ROLE", "workers")
	t.Setenv("DATABASE_URL", "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "ROLE") {
		t.Errorf("Load() error = %v, want one naming ROLE", err)
	}
}

func TestRole_Capabilities(t *testing.T) {
	for _, tc := range []struct {
		role        Role
		runsWorkers bool
		servesAPI   bool
	}{
		{RoleAll, true, true},
		{RoleAPI, false, true},
		{RoleWorker, true, false},
		// Anything Load would have rejected, the zero value included, does
		// nothing rather than everything.
		{Role(""), false, false},
		{Role("workers"), false, false},
		{Role("ALL"), false, false},
	} {
		if got := tc.role.RunsWorkers(); got != tc.runsWorkers {
			t.Errorf("Role(%q).RunsWorkers() = %v, want %v", tc.role, got, tc.runsWorkers)
		}
		if got := tc.role.ServesAPI(); got != tc.servesAPI {
			t.Errorf("Role(%q).ServesAPI() = %v, want %v", tc.role, got, tc.servesAPI)
		}
	}
}

func TestRole_ConstantsMatchTheDocumentedValues(t *testing.T) {
	if RoleAll != "all" || RoleAPI != "api" || RoleWorker != "worker" {
		t.Errorf("roles = %q %q %q, want all api worker (the values operators put in .env.host)", RoleAll, RoleAPI, RoleWorker)
	}
}

func TestLoad_InstanceIDFromEnv(t *testing.T) {
	minimalEnv(t)
	t.Setenv("INSTANCE_ID", "  worker-03 ")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InstanceID != "worker-03" {
		t.Errorf("InstanceID = %q, want %q", cfg.InstanceID, "worker-03")
	}
}

func TestLoad_InstanceIDDefaultsToHostname(t *testing.T) {
	minimalEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InstanceID == "" {
		t.Fatal("InstanceID is empty; claims and logs would carry no attribution")
	}
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		if cfg.InstanceID != strings.TrimSpace(host) {
			t.Errorf("InstanceID = %q, want the hostname %q", cfg.InstanceID, host)
		}
	}
}

func TestLoad_InstanceIDHostnameInjected(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      string
		hostname func() (string, error)
		want     string
	}{
		{"hostname", "", func() (string, error) { return "box-7", nil }, "box-7"},
		{"hostname is trimmed", "", func() (string, error) { return " box-7\n", nil }, "box-7"},
		{"blank env falls through", "   ", func() (string, error) { return "box-7", nil }, "box-7"},
		{"env wins", "named", func() (string, error) { return "box-7", nil }, "named"},
		{"hostname fails", "", func() (string, error) { return "", errors.New("no hostname") }, "unknown"},
		{"hostname fails with junk", "", func() (string, error) { return "junk", errors.New("no hostname") }, "unknown"},
		{"hostname empty", "", func() (string, error) { return "", nil }, "unknown"},
		{"hostname blank", "", func() (string, error) { return "  ", nil }, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			minimalEnv(t)
			t.Setenv("INSTANCE_ID", tc.env)
			prev := osHostname
			osHostname = tc.hostname
			t.Cleanup(func() { osHostname = prev })

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.InstanceID != tc.want {
				t.Errorf("InstanceID = %q, want %q", cfg.InstanceID, tc.want)
			}
		})
	}
}

// TestLoad_DBMaxConns mirrors db.Connect (<=0 → 10, floor 4) so the number in
// the boot log is the size of the pool that was actually opened.
func TestLoad_DBMaxConns(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{
		{"", 10},
		{"lots", 10},
		{"0", 10},
		{"-3", 10},
		{"1", 4},
		{"3", 4},
		{"4", 4},
		// First value above the floor: must pass through, not be pulled down.
		{"5", 5},
		{"6", 6},
		{"25", 25},
		// A CRLF env file or a quoted value must not turn the 6 a worker was
		// budgeted at into the default 10 (ten boxes: 100 connections, not 60).
		{"6\r", 6},
		{" 6 ", 6},
		{"3\r\n", 4},
		{"  ", 10},
	} {
		t.Run(tc.env, func(t *testing.T) {
			minimalEnv(t)
			t.Setenv("DB_MAX_CONNS", tc.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DBMaxConns != tc.want {
				t.Errorf("DB_MAX_CONNS=%q: DBMaxConns = %d, want %d", tc.env, cfg.DBMaxConns, tc.want)
			}
		})
	}
}

func TestLoad_QueueHighWater(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{
		{"", 2000},
		{"plenty", 2000},
		// An explicit 0 is "backpressure off", not "use the default".
		{"0", 0},
		{"-1", -1},
		{"500", 500},
		// Stray whitespace must not turn "off" back into the default: the
		// operator asked for no backpressure and would get 2000 with no hint.
		{"0 ", 0},
		{" 0", 0},
		{"0\r", 0},
		{" 500\n", 500},
		{" \t", 2000},
	} {
		t.Run(tc.env, func(t *testing.T) {
			minimalEnv(t)
			t.Setenv("QUEUE_HIGH_WATER", tc.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.QueueHighWater != tc.want {
				t.Errorf("QUEUE_HIGH_WATER=%q: QueueHighWater = %d, want %d", tc.env, cfg.QueueHighWater, tc.want)
			}
		})
	}
}

// TestLoad_NumbersAndBoolsIgnoreSurroundingWhitespace: ROLE and INSTANCE_ID
// are trimmed, so a CRLF .env.fleet boots — and must then mean what it says.
// The budget pair matters most: the runbook's rollback step is
// API_BUDGET_PER_CYCLE=0 + DETAILS_PER_CYCLE=0 ("spend nothing"), which an
// untrimmed parse would quietly turn back into 150 and 50.
func TestLoad_NumbersAndBoolsIgnoreSurroundingWhitespace(t *testing.T) {
	minimalEnv(t)
	t.Setenv("API_BUDGET_PER_CYCLE", "0\r")
	t.Setenv("DETAILS_PER_CYCLE", " 0 ")
	t.Setenv("SEARCH_MAX_RESULTS", "75\r")
	t.Setenv("SKIP_EXISTING", "false\r")
	t.Setenv("VIDEO_ENABLED", " false ")
	t.Setenv("VIEWER_SALT_ROTATE_DAILY", "true\r\n")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIBudgetPerCycle != 0 {
		t.Errorf(`API_BUDGET_PER_CYCLE="0\r": APIBudgetPerCycle = %d, want 0`, cfg.APIBudgetPerCycle)
	}
	if cfg.DetailsPerCycle != 0 {
		t.Errorf(`DETAILS_PER_CYCLE=" 0 ": DetailsPerCycle = %d, want 0`, cfg.DetailsPerCycle)
	}
	if cfg.Search.MaxResults != 75 {
		t.Errorf(`SEARCH_MAX_RESULTS="75\r": Search.MaxResults = %d, want 75`, cfg.Search.MaxResults)
	}
	if cfg.SkipExisting {
		t.Error(`SKIP_EXISTING="false\r": SkipExisting = true, want false`)
	}
	if cfg.Video.Enabled {
		t.Error(`VIDEO_ENABLED=" false ": Video.Enabled = true, want false`)
	}
	if !cfg.Viewer.RotateDaily {
		t.Error(`VIEWER_SALT_ROTATE_DAILY="true\r\n": Viewer.RotateDaily = false, want true`)
	}
}

// TestLoad_UnparseableNumbersAndBoolsKeepTheDefault pins the rule the trim
// must not change: a value that is still not a number (or a bool) after
// trimming falls back to the default rather than failing the boot.
func TestLoad_UnparseableNumbersAndBoolsKeepTheDefault(t *testing.T) {
	minimalEnv(t)
	t.Setenv("API_BUDGET_PER_CYCLE", "1 50")
	t.Setenv("DETAILS_PER_CYCLE", "fifty")
	t.Setenv("SKIP_EXISTING", "nope")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIBudgetPerCycle != 150 || cfg.DetailsPerCycle != 50 || !cfg.SkipExisting {
		t.Errorf("budget = %d, details = %d, skipExisting = %v, want the defaults 150, 50, true",
			cfg.APIBudgetPerCycle, cfg.DetailsPerCycle, cfg.SkipExisting)
	}
}

// TestLoad_InstanceIDNeverContainsTheOwnerSeparator: the claim owner is
// InstanceID + "/" + nonce and the runbook attributes claims with
// split_part(claimed_by, '/', 1). A slash inside the id would file
// "hel1/worker-01" and "hel1/worker-02" under one instance called "hel1".
// Control characters are replaced too: the id is printed in every log line.
func TestLoad_InstanceIDNeverContainsTheOwnerSeparator(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      string
		hostname string
		want     string
	}{
		{"slash", "hel1/worker-01", "", "hel1-worker-01"},
		{"only slashes", "//", "", "--"},
		{"leading and trailing slash", "/worker-01/", "", "-worker-01-"},
		{"newline inside", "a\nb", "", "a-b"},
		{"tab and carriage return inside", "a\tb\rc", "", "a-b-c"},
		{"delete character", "a\x7fb", "", "a-b"},
		{"edge whitespace is trimmed, not replaced", " worker-01\r\n", "", "worker-01"},
		{"hostname goes through the same filter", "", "box/7\x00", "box-7-"},
		// Left alone: neither breaks attribution nor a log line.
		{"inner space kept", "worker 01", "", "worker 01"},
		{"dots, underscores, colons kept", "w_01.hel1:a", "", "w_01.hel1:a"},
		{"non-ASCII kept", "radnik-š1", "", "radnik-š1"},
		{"backslash kept", `a\b`, "", `a\b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			minimalEnv(t)
			t.Setenv("INSTANCE_ID", tc.env)
			prev := osHostname
			osHostname = func() (string, error) { return tc.hostname, nil }
			t.Cleanup(func() { osHostname = prev })

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.InstanceID != tc.want {
				t.Errorf("InstanceID = %q, want %q", cfg.InstanceID, tc.want)
			}
			// What the runbook's split_part sees for owner = id + "/" + nonce.
			owner := cfg.InstanceID + "/3f9a"
			if got, _, _ := strings.Cut(owner, "/"); got != cfg.InstanceID {
				t.Errorf("owner %q attributes to %q, want %q", owner, got, cfg.InstanceID)
			}
		})
	}
}

// TestLoad_SearchMaxResultsDefaultsToUnlimited: a worker provisioned without
// the override must not truncate dense ZIPs and still mark them searched.
func TestLoad_SearchMaxResultsDefaultsToUnlimited(t *testing.T) {
	minimalEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Search.MaxResults != 0 {
		t.Errorf("Search.MaxResults = %d, want 0 (unlimited)", cfg.Search.MaxResults)
	}
	t.Setenv("SEARCH_MAX_RESULTS", "50")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Search.MaxResults != 50 {
		t.Errorf("Search.MaxResults = %d, want 50", cfg.Search.MaxResults)
	}
}

// TestLoad_ViewerSaltByRole: only a process that serves the API hashes
// viewers, so a worker box must boot without the secret.
func TestLoad_ViewerSaltByRole(t *testing.T) {
	for _, tc := range []struct {
		role     string
		needSalt bool
	}{
		{"", true},
		{"all", true},
		{"api", true},
		{"worker", false},
	} {
		t.Run("ROLE="+tc.role, func(t *testing.T) {
			minimalEnv(t)
			t.Setenv("ROLE", tc.role)
			t.Setenv("VIEWER_SALT", "")
			cfg, err := Load()
			switch {
			case tc.needSalt && (err == nil || !strings.Contains(err.Error(), "VIEWER_SALT")):
				t.Errorf("Load() error = %v, want one naming VIEWER_SALT", err)
			case !tc.needSalt && err != nil:
				t.Errorf("Load: %v", err)
			case !tc.needSalt && (!cfg.Linear.Enabled || !cfg.Viewer.Enabled):
				// The worker still honours LINEAR_ENABLED for segmentation;
				// loading without a salt must not switch anything off.
				t.Errorf("linear = %v, viewer = %v, want both left enabled", cfg.Linear.Enabled, cfg.Viewer.Enabled)
			}

			// With the salt present every role loads and keeps it.
			t.Setenv("VIEWER_SALT", "s3cret")
			cfg, err = Load()
			if err != nil {
				t.Fatalf("Load with salt: %v", err)
			}
			if cfg.Viewer.Salt != "s3cret" {
				t.Errorf("Viewer.Salt = %q, want %q", cfg.Viewer.Salt, "s3cret")
			}
		})
	}
}

func TestLoad_VideoThreads(t *testing.T) {
	// Unset: the machine is shared between the renders that run on it, so each
	// gets vCPUs/LISTING_CONCURRENCY threads, never fewer than one.
	t.Run("derived from listing concurrency", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("IMAGES_ENABLED", "false")
		t.Setenv("LISTING_CONCURRENCY", "4")
		c, err := Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		want := max(1, runtime.NumCPU()/4)
		if c.Video.Threads != want {
			t.Errorf("Video.Threads = %d, want %d", c.Video.Threads, want)
		}
	})
	// More renders than cores still leaves each render one thread.
	t.Run("never zero", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("IMAGES_ENABLED", "false")
		t.Setenv("LISTING_CONCURRENCY", strconv.Itoa(runtime.NumCPU()*8))
		c, err := Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if c.Video.Threads != 1 {
			t.Errorf("Video.Threads = %d, want 1", c.Video.Threads)
		}
	})
	t.Run("explicit wins", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("IMAGES_ENABLED", "false")
		t.Setenv("LISTING_CONCURRENCY", "4")
		t.Setenv("VIDEO_FFMPEG_THREADS", "6")
		c, err := Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if c.Video.Threads != 6 {
			t.Errorf("Video.Threads = %d, want 6", c.Video.Threads)
		}
	})
}
