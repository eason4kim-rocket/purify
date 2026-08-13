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

const (
	// DefaultRerankProfile names the only reference profile described by the
	// current deployment contract. Runtime certification remains a separate,
	// fail-closed gate populated only after authenticated deployment admission
	// and acceptance of its matching recording manifest.
	DefaultRerankProfile = "qwen3-reranker-0.6b-v1"

	maximumRerankEndpointBytes = 16 << 10
	maximumRerankAPIKeyBytes   = 16 << 10
	maximumRerankProfileBytes  = 128
)

var ErrInvalidCompilerConfig = errors.New("config: invalid managed compiler configuration")

// ErrInvalidRerankConfig marks unusable process-owned reranker settings.
var ErrInvalidRerankConfig = errors.New("config: invalid reranker configuration")

// ErrInvalidEAVConfig marks unusable entity-attribution settings.
var ErrInvalidEAVConfig = errors.New("config: invalid entity attribution configuration")

// maximumEAVCacheEntries bounds the in-process blind-extraction cache.
const maximumEAVCacheEntries = 4096

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
	EAV          EAVConfig
	Heal         HealConfig
	Search       SearchConfig
	Rerank       RerankConfig
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

// EAVConfig controls process-owned entity-attribution judging for
// multi-source extraction. The credential is used only by the managed judge
// and is never a fallback for request-scoped extraction.
type EAVConfig struct {
	Enabled        bool
	RefereeEnabled bool
	CacheEntries   int
	APIKey         string
	Model          string
	BaseURL        string
	// AllowPrivate permits only the process-owned EAV provider to resolve and
	// dial private networks. Request-scoped LLM providers remain public-only.
	AllowPrivate bool
}

// HealConfig controls the optional process-owned lifecycle webhook emitted by
// verified extractor healing. Healing itself is enabled by durable snapshots,
// independently of managed compiler synthesis.
type HealConfig struct {
	WebhookURL    string
	WebhookSecret string
}

// SearchConfig controls the process-owned baseline Search provider. An empty
// index path leaves Search unavailable without constructing provider state.
type SearchConfig struct {
	IndexPath string
	// FeedIndex adds pages fetched while serving a request to the index. The
	// pages are public web content owned by their publisher rather than by the
	// caller, and private addresses never reach the fetcher, so this is on by
	// default. What it does leak is the URL set: an index that suddenly fills
	// with one company's pages shows that someone is researching it.
	FeedIndex bool
}

// RerankConfig controls the process-owned metadata reranker. It only describes
// operator-observable settings; a future enabled runtime must separately pass
// authenticated deployment admission and the matching recorded-profile gate.
type RerankConfig struct {
	Enabled        bool
	Endpoint       string
	APIKey         string
	Profile        string
	AllowPrivate   bool
	TimeoutSeconds int
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

	// EnableArchiveFallback adds a last-resort Wayback Machine fetch when every
	// live engine fails, so a blocked or down origin can still yield content.
	// Archive results are transparently labeled and never fed to the index.
	EnableArchiveFallback bool // default: true

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

	// DefaultProxy is the default proxy URL for all requests. It also backs the
	// browser launch proxy, so Rod keeps a single process-lifetime relay.
	DefaultProxy string

	// ProxyPool rotates egress across several proxy URLs for the HTTP engine.
	// With zero or one entry the engine keeps its single shared client; with two
	// or more it round-robins a per-request client across them. Rod continues to
	// use DefaultProxy so it never pays a per-request relay.
	ProxyPool []string

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
			ProxyPool:    envSliceOr("PURIFY_PROXY_POOL", nil),
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
			EnableMultiEngine:     envBoolOr("PURIFY_MULTI_ENGINE", true),
			EnableArchiveFallback: envBoolOr("PURIFY_ARCHIVE_FALLBACK", true),
			EscalationDelays:      envDurationSliceOr("PURIFY_ESCALATION_DELAYS", []time.Duration{0, 2 * time.Second, 5 * time.Second}),
			HTTPTimeout:           envDurationOr("PURIFY_HTTP_TIMEOUT", 5*time.Second),
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
		EAV: EAVConfig{
			Enabled:        envBoolOr("PURIFY_EAV_ENABLED", false),
			RefereeEnabled: envBoolOr("PURIFY_EAV_REFEREE_ENABLED", true),
			CacheEntries:   envIntOr("PURIFY_EAV_CACHE_ENTRIES", 128),
			APIKey:         os.Getenv("PURIFY_EAV_LLM_API_KEY"),
			Model:          envOr("PURIFY_EAV_LLM_MODEL", "gpt-4o-mini"),
			BaseURL:        envOr("PURIFY_EAV_LLM_BASE_URL", "https://api.openai.com/v1"),
			AllowPrivate:   envBoolOr("PURIFY_EAV_LLM_ALLOW_PRIVATE", false),
		},
		Heal: HealConfig{
			WebhookURL:    os.Getenv("PURIFY_HEAL_WEBHOOK_URL"),
			WebhookSecret: os.Getenv("PURIFY_HEAL_WEBHOOK_SECRET"),
		},
		Search: SearchConfig{
			IndexPath: os.Getenv("PURIFY_SEARCH_INDEX_PATH"),
			FeedIndex: envBoolOr("PURIFY_SEARCH_INDEX_FEED", true),
		},
		Rerank: RerankConfig{
			Enabled:        envBoolOr("PURIFY_RERANK_ENABLED", false),
			Endpoint:       os.Getenv("PURIFY_RERANK_ENDPOINT"),
			APIKey:         os.Getenv("PURIFY_RERANK_API_KEY"),
			Profile:        envOr("PURIFY_RERANK_PROFILE", DefaultRerankProfile),
			AllowPrivate:   envBoolOr("PURIFY_RERANK_ALLOW_PRIVATE", false),
			TimeoutSeconds: envStrictDecimalIntOr("PURIFY_RERANK_TIMEOUT_SECONDS", 5),
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
	if err := validateProviderCredential(value.APIKey); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCompilerConfig, err)
	}
	if !snapshotEnabled {
		return fmt.Errorf("%w: snapshots must be enabled", ErrInvalidCompilerConfig)
	}
	if err := validateProviderModel(value.Model); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCompilerConfig, err)
	}
	if err := validateProviderBaseURL(value.BaseURL); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCompilerConfig, err)
	}
	return nil
}

// ValidateEAVConfig rejects an enabled entity-attribution judge whose
// provider settings or cache bound are unusable. Disabled configuration is
// inert, including any stale provider variables left in the environment.
func ValidateEAVConfig(value EAVConfig) error {
	if !value.Enabled {
		return nil
	}
	if err := validateProviderCredential(value.APIKey); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEAVConfig, err)
	}
	if err := validateProviderModel(value.Model); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEAVConfig, err)
	}
	if err := validateProviderBaseURL(value.BaseURL); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEAVConfig, err)
	}
	if value.CacheEntries < 1 || value.CacheEntries > maximumEAVCacheEntries {
		return fmt.Errorf("%w: cache entries must be between 1 and %d", ErrInvalidEAVConfig, maximumEAVCacheEntries)
	}
	return nil
}

// ValidateRerankConfig validates only the process-observable syntax and
// resource bounds. It deliberately does not treat an operator profile string
// as proof of the model, template, image, or recording manifest behind it.
// Disabled configuration is inert, including stale or malformed provider
// values left in the environment.
func ValidateRerankConfig(value RerankConfig) error {
	if !value.Enabled {
		return nil
	}
	if err := validateRerankEndpoint(value.Endpoint, value.AllowPrivate); err != nil {
		return fmt.Errorf("%w: endpoint is invalid", ErrInvalidRerankConfig)
	}
	if !validRerankString(value.APIKey, maximumRerankAPIKeyBytes) {
		return fmt.Errorf("%w: API key is invalid", ErrInvalidRerankConfig)
	}
	if !validRerankString(value.Profile, maximumRerankProfileBytes) {
		return fmt.Errorf("%w: profile is invalid", ErrInvalidRerankConfig)
	}
	if value.TimeoutSeconds < 1 || value.TimeoutSeconds > 10 {
		return fmt.Errorf("%w: timeout must be between 1 and 10 seconds", ErrInvalidRerankConfig)
	}
	return nil
}

func validateRerankEndpoint(endpoint string, allowPrivate bool) error {
	if !validRerankString(endpoint, maximumRerankEndpointBytes) {
		return ErrInvalidRerankConfig
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		strings.Contains(endpoint, "#") || parsed.Path != "/v1/rerank" || parsed.RawPath != "" ||
		parsed.EscapedPath() != "/v1/rerank" || strings.HasSuffix(parsed.Host, ":") ||
		strings.Contains(parsed.Hostname(), "%") {
		return ErrInvalidRerankConfig
	}
	scheme := strings.ToLower(parsed.Scheme)
	if parsed.Scheme != scheme || parsed.String() != endpoint ||
		scheme != "http" && scheme != "https" || scheme == "http" && !allowPrivate {
		return ErrInvalidRerankConfig
	}
	if port := parsed.Port(); port != "" {
		numericPort, convertErr := strconv.Atoi(port)
		if convertErr != nil || numericPort < 1 || numericPort > 65_535 {
			return ErrInvalidRerankConfig
		}
	}
	return nil
}

func validRerankString(value string, maximumBytes int) bool {
	if value == "" || len(value) > maximumBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validateProviderCredential(apiKey string) error {
	if len(apiKey) > maximumCompilerCredentialBytes || strings.TrimSpace(apiKey) == "" {
		return errors.New("API key is invalid")
	}
	return nil
}

func validateProviderModel(rawModel string) error {
	if len(rawModel) > maximumCompilerModelBytes {
		return errors.New("model is invalid")
	}
	model := strings.TrimSpace(rawModel)
	if model == "" || len(model) > maximumCompilerModelBytes {
		return errors.New("model is invalid")
	}
	for _, character := range model {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return errors.New("model is invalid")
		}
	}
	return nil
}

func validateProviderBaseURL(rawBaseURL string) error {
	if len(rawBaseURL) > maximumCompilerBaseURLBytes {
		return errors.New("base URL is invalid")
	}
	baseURL := strings.TrimSpace(rawBaseURL)
	parsed, err := url.Parse(baseURL)
	validScheme := err == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https"))
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" ||
		!validScheme || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("base URL is invalid")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return errors.New("base URL is invalid")
	}
	if port := parsed.Port(); port != "" {
		numericPort, err := strconv.Atoi(port)
		if err != nil || numericPort < 1 || numericPort > 65535 {
			return errors.New("base URL is invalid")
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

// envStrictDecimalIntOr accepts only an unadorned ASCII base-10 integer. A
// malformed configured value maps to zero so an enabled feature's validator
// fails closed; an unset or explicitly empty value keeps the documented
// default. Disabled feature validation remains deliberately inert.
func envStrictDecimalIntOr(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	for _, character := range raw {
		if character < '0' || character > '9' {
			return 0
		}
	}
	value, err := strconv.ParseUint(raw, 10, 31)
	if err != nil {
		return 0
	}
	return int(value)
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
