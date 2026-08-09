package publicnet

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// NormalizeHTTPURL resolves rawURL against base, validates HTTP(S) URL syntax,
// and returns a canonical URL. Literal IP hosts are checked immediately.
// Domain names are deliberately not resolved here: callers must use Resolve or
// DialContext at connection time to obtain DNS rebinding protection.
func NormalizeHTTPURL(rawURL string, base *url.URL, allowPrivateNetworks bool) (string, *url.URL, error) {
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
	if hostname == "" || strings.Contains(hostname, "%") {
		return "", nil, errors.New("URL host is invalid")
	}
	canonicalHost := ""
	if address, parseErr := netip.ParseAddr(hostname); parseErr == nil {
		if address.Zone() != "" {
			return "", nil, errors.New("IPv6 zones are not supported")
		}
		if !allowPrivateNetworks && !IsPublicAddress(address) {
			return "", nil, errors.New("private or local address is not allowed")
		}
		canonicalHost = address.String()
	} else {
		canonicalHost, err = idna.Lookup.ToASCII(hostname)
		if err != nil {
			return "", nil, fmt.Errorf("normalize host: %w", err)
		}
		canonicalHost = strings.TrimSuffix(strings.ToLower(canonicalHost), ".")
		if !allowPrivateNetworks && (canonicalHost == "localhost" || strings.HasSuffix(canonicalHost, ".localhost")) {
			return "", nil, errors.New("localhost is not allowed")
		}
	}

	port := parsed.Port()
	if port == "" && explicitEmptyPort(parsed.Host) {
		return "", nil, errors.New("URL port is empty")
	}
	if port != "" {
		number, convertErr := strconv.Atoi(port)
		if convertErr != nil || number < 1 || number > 65_535 {
			return "", nil, errors.New("URL port must be between 1 and 65535")
		}
		port = strconv.Itoa(number)
		if scheme == "http" && port == "80" || scheme == "https" && port == "443" {
			port = ""
		}
	}
	parsed.Scheme = scheme
	switch {
	case port != "":
		if strings.Contains(canonicalHost, ":") {
			parsed.Host = "[" + canonicalHost + "]:" + port
		} else {
			parsed.Host = canonicalHost + ":" + port
		}
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

func explicitEmptyPort(authority string) bool {
	if strings.HasPrefix(authority, "[") {
		closing := strings.LastIndex(authority, "]")
		return closing >= 0 && authority[closing+1:] == ":"
	}
	return strings.HasSuffix(authority, ":")
}

// IsPublicAddress reports whether address is globally reachable and is not in
// a local, documentation, benchmarking, shared, or otherwise reserved range.
func IsPublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range reservedAddressPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var reservedAddressPrefixes = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
}
