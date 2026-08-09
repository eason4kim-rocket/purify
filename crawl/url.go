package crawl

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

var (
	// ErrInvalidURL is returned when the crawl root is not a canonicalizable
	// absolute HTTP(S) URL.
	ErrInvalidURL = errors.New("crawl: invalid URL")
	// ErrInvalidExcludePattern is returned when an exclude glob is malformed.
	ErrInvalidExcludePattern = errors.New("crawl: invalid exclude pattern")
)

type scopeRule struct {
	mode        string
	authority   string
	hostname    string
	registrable string
	exactOnly   bool
}

func normalizeRootURL(rawURL string) (string, *url.URL, error) {
	canonical, parsed, err := normalizeURL(rawURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	return canonical, parsed, nil
}

func normalizeURL(rawURL string, base *url.URL) (string, *url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", nil, errors.New("URL is empty")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse URL: %w", err)
	}
	if base != nil && !parsed.IsAbs() {
		parsed = base.ResolveReference(parsed)
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return "", nil, errors.New("URL must be absolute")
	}
	if parsed.Opaque != "" {
		return "", nil, errors.New("opaque URLs are not supported")
	}
	if parsed.User != nil {
		return "", nil, errors.New("URL userinfo is not supported")
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", nil, errors.New("URL scheme must be http or https")
	}

	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "" {
		return "", nil, errors.New("URL host is empty")
	}
	if strings.Contains(hostname, "%") {
		return "", nil, errors.New("IPv6 zones are not supported")
	}

	canonicalHost := ""
	if address, parseErr := netip.ParseAddr(hostname); parseErr == nil {
		if address.Zone() != "" {
			return "", nil, errors.New("IPv6 zones are not supported")
		}
		canonicalHost = address.String()
	} else {
		canonicalHost, err = idna.Lookup.ToASCII(hostname)
		if err != nil {
			return "", nil, fmt.Errorf("normalize internationalized host: %w", err)
		}
		canonicalHost = strings.TrimSuffix(strings.ToLower(canonicalHost), ".")
		if canonicalHost == "" {
			return "", nil, errors.New("URL host is empty")
		}
	}

	port := parsed.Port()
	if port == "" && hasExplicitEmptyPort(parsed.Host) {
		return "", nil, errors.New("URL port is empty")
	}
	if port != "" {
		portNumber, parseErr := strconv.Atoi(port)
		if parseErr != nil || portNumber < 1 || portNumber > 65_535 {
			return "", nil, errors.New("URL port must be between 1 and 65535")
		}
		port = strconv.Itoa(portNumber)
		if scheme == "http" && port == "80" || scheme == "https" && port == "443" {
			port = ""
		}
	}

	parsed.Scheme = scheme
	switch {
	case port != "":
		parsed.Host = netJoinHostPort(canonicalHost, port)
	case strings.Contains(canonicalHost, ":"):
		parsed.Host = "[" + canonicalHost + "]"
	default:
		parsed.Host = canonicalHost
	}
	if parsed.Path == "" {
		parsed.Path = "/"
		parsed.RawPath = ""
	}
	parsed.Fragment = ""
	parsed.RawFragment = ""

	return parsed.String(), parsed, nil
}

func netJoinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

func hasExplicitEmptyPort(authority string) bool {
	if strings.HasPrefix(authority, "[") {
		closing := strings.LastIndex(authority, "]")
		return closing >= 0 && authority[closing+1:] == ":"
	}
	return strings.HasSuffix(authority, ":")
}

func newScopeRule(mode string, root *url.URL) scopeRule {
	rule := scopeRule{
		mode:      mode,
		authority: root.Host,
		hostname:  root.Hostname(),
	}
	if mode != scopeSubdomain {
		return rule
	}
	if _, err := netip.ParseAddr(rule.hostname); err == nil || strings.EqualFold(rule.hostname, "localhost") {
		rule.exactOnly = true
		return rule
	}
	registrable, err := publicsuffix.EffectiveTLDPlusOne(rule.hostname)
	if err != nil {
		rule.exactOnly = true
		return rule
	}
	rule.registrable = strings.ToLower(registrable)
	return rule
}

func (rule scopeRule) allows(candidate *url.URL) bool {
	if candidate == nil {
		return false
	}
	switch rule.mode {
	case scopePage:
		return false
	case scopeDomain:
		return candidate.Host == rule.authority
	case scopeSubdomain:
		if rule.exactOnly {
			return strings.EqualFold(candidate.Hostname(), rule.hostname)
		}
		registrable, err := publicsuffix.EffectiveTLDPlusOne(candidate.Hostname())
		return err == nil && strings.EqualFold(registrable, rule.registrable)
	default:
		return false
	}
}

func validateExcludePatterns(patterns []string) error {
	if len(patterns) > maximumExcludeCount {
		return fmt.Errorf("%w: at most %d patterns are allowed", ErrInvalidExcludePattern, maximumExcludeCount)
	}
	for _, pattern := range patterns {
		if len(pattern) > maximumExcludeLength {
			return fmt.Errorf("%w: pattern exceeds %d bytes", ErrInvalidExcludePattern, maximumExcludeLength)
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("%w %q: %v", ErrInvalidExcludePattern, pattern, err)
		}
	}
	return nil
}

func isExcluded(canonical string, parsed *url.URL, patterns []string) bool {
	if parsed == nil {
		return false
	}
	pathWithoutRoot := strings.TrimPrefix(parsed.Path, "/")
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		for _, candidate := range []string{parsed.Path, pathWithoutRoot, path.Base(parsed.Path), canonical} {
			if matched, _ := path.Match(pattern, candidate); matched {
				return true
			}
		}
	}
	return false
}
