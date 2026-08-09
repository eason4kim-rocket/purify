package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/publicnet"
)

const maximumCompilerModelBytes = 256
const maximumCompilerCredentialBytes = 16 << 10
const maximumCompilerBaseURLBytes = 16 << 10

var ErrInvalidCompilerConfig = errors.New("config: invalid managed compiler configuration")
var ErrInvalidHealConfig = errors.New("config: invalid extractor heal configuration")

// Config holds all application configuration.
type Config struct {
	Server       ServerConfig
	Browser      BrowserConfig
	Scraper      ScraperConfig
	Auth         AuthConfig
	RateLimit    RateLimitConfig
	Cache        CacheConfig
	Log          LogConfig
	Engine       EngineConfig
	AdaptivePool AdaptivePoolConfig
	Storage      StorageConfig
	Compiler     CompilerConfig
	Heal         HealConfig
	Search       SearchConfig
}

// CompilerConfig controls process-owned background extractor synthesis. The
// credential is used only by the managed compiler and is never a fallback for
// request-scoped extraction.
type CompilerConfig struct {
	Enabled bool
	APIKey  string
	Model   string
	BaseURL string
}

// HealConfig controls the optional process-owned lifecycle webhook emitted by
// verified extractor healing. Healing itself is enabled by durable snapshots,
// independently of managed compiler synthesis.
type HealConfig struct {
	WebhookURL    string
	WebhookSecret string
}

// SearchConfig controls the process-owned baseline Search provider. An empty
// credential leaves Search unavailable without constructing provider state.
type SearchConfig struct {
	BraveKey string
}

// StorageConfig controls durable snapshots, the ledger, and receipt signing.
type StorageConfig struct {
	DataDir         string // default: "./data"
	SnapshotEnabled bool   // default: true
	SigningKey      string // optional Ed25519 seed encoded as hex
}

// EngineConfig controls the multi-engine racing dispatcher.
type EngineConfig struct {
	// EnableMultiEngine toggles the multi-engine dispatcher.
	EnableMultiEngine bool // default: true

	// EscalationDelays is the staged start delay for each engine tier.
	EscalationDelays []time.Duration // default: [0s, 2s, 5s]

	// HTTPTimeout is the deadline for the pure HTTP engine.
	HTTPTimeout time.Duration // default: 5s
}

// AdaptivePoolConfig controls the adaptive page pool sizing.
type AdaptivePoolConfig struct {
	// MinPages is the minimum number of pages kept in the pool.
	MinPages int // default: 3

	// HardMax is the absolute maximum number of pages.
	HardMax int // default: 20

	// MemThreshold is the heap memory fraction (0.0-1.0) above which the pool shrinks.
	MemThreshold float64 // default: 0.9

	// ScaleStep is the fraction of pool size to grow or shrink per interval.
	ScaleStep float64 // default: 0.05
}

// CacheConfig controls the scrape response cache.
type CacheConfig struct {
	// MaxEntries is the maximum number of cached responses.
	MaxEntries int // default: 1000
}

// ServerConfig controls the HTTP server.
type ServerConfig struct {
	Host string // default: "0.0.0.0"
	Port int    // default: 8080
	Mode string // "debug", "release", "test"; default: "release"
}

// BrowserConfig controls the Rod browser instance.
type BrowserConfig struct {
	// Headless controls whether the browser runs headless.
	Headless bool // default: true

	// MaxPages is the page pool capacity (max concurrent tabs).
	MaxPages int // default: 10

	// DefaultProxy is the default proxy URL for all requests.
	DefaultProxy string

	// NoSandbox disables Chrome's sandbox (needed in Docker).
	NoSandbox bool // default: false

	// BrowserBin overrides the Chromium binary path.
	BrowserBin string
}

// ScraperConfig controls scraping behavior.
type ScraperConfig struct {
	// DefaultTimeout is the per-request timeout.
	DefaultTimeout time.Duration // default: 30s

	// MaxTimeout is the maximum allowed timeout from the client.
	MaxTimeout time.Duration // default: 120s

	// NavigationTimeout is the max time for page.Navigate alone.
	NavigationTimeout time.Duration // default: 15s

	// BlockedResourceTypes lists resource types to block.
	// default: ["Image", "Stylesheet", "Font", "Media"]
	BlockedResourceTypes []string
}

// AuthConfig controls API key authentication.
type AuthConfig struct {
	// Enabled toggles API key authentication.
	Enabled bool // default: true

	// APIKeys is the list of valid API keys (for MVP; replace with DB later).
	APIKeys []string
}

// RateLimitConfig controls per-key rate limiting.
type RateLimitConfig struct {
	// RequestsPerSecond is the sustained rate per API key.
	RequestsPerSecond float64 // default: 5

	// Burst is the maximum burst size per API key.
	Burst int // default: 10
}

// LogConfig controls structured logging.
type LogConfig struct {
	Level  string // default: "info"
	Format string // "json" or "text"; default: "json"
}

// Load reads configuration from environment variables with sane defaults.
func Load() *Config {
	return &Config{
		Server: ServerConfig{
			Host: envOr("PURIFY_HOST", "0.0.0.0"),
			Port: envIntOr("PURIFY_PORT", 8080),
			Mode: envOr("PURIFY_MODE", "release"),
		},
		Browser: BrowserConfig{
			Headless:     envBoolOr("PURIFY_HEADLESS", true),
			MaxPages:     envIntOr("PURIFY_MAX_PAGES", 10),
			DefaultProxy: os.Getenv("PURIFY_PROXY"),
			NoSandbox:    envBoolOr("PURIFY_NO_SANDBOX", false),
			BrowserBin:   os.Getenv("PURIFY_BROWSER_BIN"),
		},
		Scraper: ScraperConfig{
			DefaultTimeout:    envDurationOr("PURIFY_DEFAULT_TIMEOUT", 30*time.Second),
			MaxTimeout:        envDurationOr("PURIFY_MAX_TIMEOUT", 120*time.Second),
			NavigationTimeout: envDurationOr("PURIFY_NAV_TIMEOUT", 15*time.Second),
			BlockedResourceTypes: envSliceOr("PURIFY_BLOCKED_RESOURCES", []string{
				"Image", "Stylesheet", "Font", "Media",
			}),
		},
		Auth: AuthConfig{
			Enabled: envBoolOr("PURIFY_AUTH_ENABLED", true),
			APIKeys: envSliceOr("PURIFY_API_KEYS", nil),
		},
		RateLimit: RateLimitConfig{
			RequestsPerSecond: envFloatOr("PURIFY_RATE_RPS", 5.0),
			Burst:             envIntOr("PURIFY_RATE_BURST", 10),
		},
		Cache: CacheConfig{
			MaxEntries: envIntOr("CACHE_MAX_ENTRIES", 1000),
		},
		Log: LogConfig{
			Level:  envOr("PURIFY_LOG_LEVEL", "info"),
			Format: envOr("PURIFY_LOG_FORMAT", "json"),
		},
		Engine: EngineConfig{
			EnableMultiEngine: envBoolOr("PURIFY_MULTI_ENGINE", true),
			EscalationDelays:  envDurationSliceOr("PURIFY_ESCALATION_DELAYS", []time.Duration{0, 2 * time.Second, 5 * time.Second}),
			HTTPTimeout:       envDurationOr("PURIFY_HTTP_TIMEOUT", 5*time.Second),
		},
		AdaptivePool: AdaptivePoolConfig{
			MinPages:     envIntOr("PURIFY_MIN_PAGES", 3),
			HardMax:      envIntOr("PURIFY_HARD_MAX_PAGES", 20),
			MemThreshold: envFloatOr("PURIFY_MEM_THRESHOLD", 0.9),
			ScaleStep:    envFloatOr("PURIFY_SCALE_STEP", 0.05),
		},
		Storage: StorageConfig{
			DataDir:         envOr("PURIFY_DATA_DIR", "./data"),
			SnapshotEnabled: envBoolOr("PURIFY_SNAPSHOT_ENABLED", true),
			SigningKey:      os.Getenv("PURIFY_SIGNING_KEY"),
		},
		Compiler: CompilerConfig{
			Enabled: envBoolOr("PURIFY_COMPILER_ENABLED", false),
			APIKey:  os.Getenv("PURIFY_COMPILER_API_KEY"),
			Model:   envOr("PURIFY_COMPILER_MODEL", "gpt-4o-mini"),
			BaseURL: envOr("PURIFY_COMPILER_BASE_URL", "https://api.openai.com/v1"),
		},
		Heal: HealConfig{
			WebhookURL:    os.Getenv("PURIFY_HEAL_WEBHOOK_URL"),
			WebhookSecret: os.Getenv("PURIFY_HEAL_WEBHOOK_SECRET"),
		},
		Search: SearchConfig{
			BraveKey: os.Getenv("PURIFY_SEARCH_BRAVE_KEY"),
		},
	}
}

// ValidateHealConfig rejects configured healing delivery when durable
// snapshots are unavailable. A completely empty configuration remains inert
// with snapshots off; a secret without a destination is always invalid.
func ValidateHealConfig(value HealConfig, snapshotEnabled bool) error {
	if value.WebhookURL == "" && value.WebhookSecret != "" {
		return fmt.Errorf("%w: webhook secret requires a destination", ErrInvalidHealConfig)
	}
	if !snapshotEnabled {
		if value.WebhookURL == "" && value.WebhookSecret == "" {
			return nil
		}
		return fmt.Errorf("%w: snapshots must be enabled", ErrInvalidHealConfig)
	}
	if value.WebhookURL == "" {
		return nil
	}
	if len(value.WebhookURL) > ledger.MaxOutboxURLBytes ||
		len(value.WebhookSecret) > ledger.MaxOutboxSecretBytes ||
		!utf8.ValidString(value.WebhookURL) || !utf8.ValidString(value.WebhookSecret) {
		return fmt.Errorf("%w: webhook configuration exceeds its resource limit", ErrInvalidHealConfig)
	}
	canonical, _, err := publicnet.NormalizeHTTPURL(value.WebhookURL, nil, false)
	if err != nil || len(canonical) > ledger.MaxOutboxURLBytes {
		return fmt.Errorf("%w: webhook destination is invalid", ErrInvalidHealConfig)
	}
	return nil
}

// ValidateCompilerConfig rejects an enabled managed compiler that cannot
// safely synthesize from durable snapshots. Disabled configuration is inert,
// including any stale provider variables left in the environment.
func ValidateCompilerConfig(value CompilerConfig, snapshotEnabled bool) error {
	if !value.Enabled {
		return nil
	}
	if len(value.APIKey) > maximumCompilerCredentialBytes || strings.TrimSpace(value.APIKey) == "" {
		return fmt.Errorf("%w: API key is invalid", ErrInvalidCompilerConfig)
	}
	if !snapshotEnabled {
		return fmt.Errorf("%w: snapshots must be enabled", ErrInvalidCompilerConfig)
	}

	if len(value.Model) > maximumCompilerModelBytes {
		return fmt.Errorf("%w: model is invalid", ErrInvalidCompilerConfig)
	}
	model := strings.TrimSpace(value.Model)
	if model == "" || len(model) > maximumCompilerModelBytes {
		return fmt.Errorf("%w: model is invalid", ErrInvalidCompilerConfig)
	}
	for _, character := range model {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf("%w: model is invalid", ErrInvalidCompilerConfig)
		}
	}

	if len(value.BaseURL) > maximumCompilerBaseURLBytes {
		return fmt.Errorf("%w: base URL is invalid", ErrInvalidCompilerConfig)
	}
	baseURL := strings.TrimSpace(value.BaseURL)
	parsed, err := url.Parse(baseURL)
	validScheme := err == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https"))
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" ||
		!validScheme || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("%w: base URL is invalid", ErrInvalidCompilerConfig)
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return fmt.Errorf("%w: base URL is invalid", ErrInvalidCompilerConfig)
	}
	if port := parsed.Port(); port != "" {
		numericPort, err := strconv.Atoi(port)
		if err != nil || numericPort < 1 || numericPort > 65535 {
			return fmt.Errorf("%w: base URL is invalid", ErrInvalidCompilerConfig)
		}
	}
	return nil
}

func envDurationSliceOr(key string, fallback []time.Duration) []time.Duration {
	if v := os.Getenv(key); v != "" {
		parts := strings.Split(v, ",")
		result := make([]time.Duration, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				if d, err := time.ParseDuration(trimmed); err == nil {
					result = append(result, d)
				}
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	return fallback
}

// --- helper functions ---

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func envBoolOr(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envFloatOr(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envSliceOr(key string, fallback []string) []string {
	if v := os.Getenv(key); v != "" {
		parts := strings.Split(v, ",")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				result = append(result, trimmed)
			}
		}
		return result
	}
	return fallback
}
