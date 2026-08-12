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
	if address.Is6() {
		// RFC 6052's well-known NAT64 prefix is globally reachable only when
		// its embedded IPv4 destination is itself public. This prevents a DNS
		// answer from smuggling a private IPv4 target through NAT64.
		if nat64WellKnownPrefix.Contains(address) {
			bytes := address.As16()
			return IsPublicAddress(netip.AddrFrom4([4]byte{bytes[12], bytes[13], bytes[14], bytes[15]}))
		}
		// A few special-purpose allocations are explicitly globally reachable,
		// including the AS112-v6 service prefix.
		for _, prefix := range globallyReachableSpecialAddressPrefixes {
			if prefix.Contains(address) {
				return true
			}
		}
		// Go's IsGlobalUnicast deliberately includes deprecated site-local and
		// unallocated IPv6 space. Fail closed to IANA's current allocated set.
		allocated := false
		for _, prefix := range globallyAllocatedIPv6Prefixes {
			if prefix.Contains(address) {
				allocated = true
				break
			}
		}
		if !allocated {
			return false
		}
	}
	for _, prefix := range reservedAddressPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var nat64WellKnownPrefix = netip.MustParsePrefix("64:ff9b::/96")

// Synchronized with the IANA IPv6 Special-Purpose Address Registry updated
// 2025-10-09. These are the globally reachable allocations nested inside the
// otherwise non-global 2001::/23 protocol-assignment block, plus AS112-v6.
var globallyReachableSpecialAddressPrefixes = [...]netip.Prefix{
	netip.MustParsePrefix("2001:1::1/128"),
	netip.MustParsePrefix("2001:1::2/128"),
	netip.MustParsePrefix("2001:1::3/128"),
	netip.MustParsePrefix("2001:3::/32"),
	netip.MustParsePrefix("2001:4:112::/48"),
	netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("2001:30::/28"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
}

// Synchronized with the IANA IPv6 Global Unicast Address Space registry
// updated 2025-10-10. Unlisted IPv6 space fails closed even though Go's
// IsGlobalUnicast reports most non-multicast addresses as global unicast.
var globallyAllocatedIPv6Prefixes = [...]netip.Prefix{
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:200::/23"),
	netip.MustParsePrefix("2001:400::/23"),
	netip.MustParsePrefix("2001:600::/23"),
	netip.MustParsePrefix("2001:800::/22"),
	netip.MustParsePrefix("2001:c00::/23"),
	netip.MustParsePrefix("2001:e00::/23"),
	netip.MustParsePrefix("2001:1200::/23"),
	netip.MustParsePrefix("2001:1400::/22"),
	netip.MustParsePrefix("2001:1800::/23"),
	netip.MustParsePrefix("2001:1a00::/23"),
	netip.MustParsePrefix("2001:1c00::/22"),
	netip.MustParsePrefix("2001:2000::/19"),
	netip.MustParsePrefix("2001:4000::/23"),
	netip.MustParsePrefix("2001:4200::/23"),
	netip.MustParsePrefix("2001:4400::/23"),
	netip.MustParsePrefix("2001:4600::/23"),
	netip.MustParsePrefix("2001:4800::/23"),
	netip.MustParsePrefix("2001:4a00::/23"),
	netip.MustParsePrefix("2001:4c00::/23"),
	netip.MustParsePrefix("2001:5000::/20"),
	netip.MustParsePrefix("2001:8000::/19"),
	netip.MustParsePrefix("2001:a000::/20"),
	netip.MustParsePrefix("2001:b000::/20"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2003::/18"),
	netip.MustParsePrefix("2400::/12"),
	netip.MustParsePrefix("2410::/12"),
	netip.MustParsePrefix("2600::/12"),
	netip.MustParsePrefix("2610::/23"),
	netip.MustParsePrefix("2620::/23"),
	netip.MustParsePrefix("2630::/12"),
	netip.MustParsePrefix("2800::/12"),
	netip.MustParsePrefix("2a00::/12"),
	netip.MustParsePrefix("2a10::/12"),
	netip.MustParsePrefix("2c00::/12"),
}

// Synchronized with the IANA IPv4 and IPv6 Special-Purpose Address
// Registries updated 2025-10-09. Broad protocol-assignment ranges are denied
// conservatively; the explicit globally reachable IPv6 exceptions are above.
var reservedAddressPrefixes = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("100:0:0:1::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
}
