// Package publicnet provides reusable public-only outbound network policy.
//
// Policy resolves hostnames at dial time, rejects an entire DNS answer when
// any candidate is not public, and passes only validated literal IP addresses
// to the underlying dialer. This prevents DNS rebinding between validation and
// connection establishment.
package publicnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/idna"
)

const (
	defaultDialTimeout = 10 * time.Second
	defaultKeepAlive   = 30 * time.Second
)

// ErrNotPublic marks a target rejected by the public-only policy.
var ErrNotPublic = errors.New("publicnet: target is not public")

// Resolver is the DNS boundary used by Policy.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// DialContextFunc is the network connection boundary used by Policy.
type DialContextFunc func(context.Context, string, string) (net.Conn, error)

// Options configures a Policy. Nil Resolver and DialContext values select
// net.DefaultResolver and a context-aware net.Dialer with bounded defaults.
// AllowPrivateNetworks exists for callers that explicitly need the legacy
// opt-out; its zero value enforces public-only networking.
type Options struct {
	AllowPrivateNetworks bool
	Resolver             Resolver
	DialContext          DialContextFunc
}

// Policy resolves and pins outbound TCP connections to validated IPs.
type Policy struct {
	allowPrivate bool
	resolver     Resolver
	dial         DialContextFunc
}

// NewPolicy returns an immutable, concurrency-safe outbound policy.
func NewPolicy(options Options) *Policy {
	resolver := options.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dial := options.DialContext
	if dial == nil {
		networkDialer := &net.Dialer{Timeout: defaultDialTimeout, KeepAlive: defaultKeepAlive}
		dial = networkDialer.DialContext
	}
	return &Policy{
		allowPrivate: options.AllowPrivateNetworks,
		resolver:     resolver,
		dial:         dial,
	}
}

// Resolve resolves hostname and validates the complete answer before returning
// any address. A mixed public/private answer is rejected in public-only mode.
// Every call performs a fresh lookup so callers can enforce policy per dial.
func (policy *Policy) Resolve(ctx context.Context, hostname string) ([]netip.Addr, error) {
	if policy == nil {
		return nil, errors.New("publicnet: policy is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if hostname == "" {
		return nil, errors.New("publicnet: target host is empty")
	}
	if address, err := netip.ParseAddr(hostname); err == nil {
		if address.Zone() != "" {
			return nil, errors.New("publicnet: IPv6 zones are not supported")
		}
		address = address.Unmap()
		if !policy.allowPrivate && !IsPublicAddress(address) {
			return nil, fmt.Errorf("%w: address %s is private, local, or reserved", ErrNotPublic, address)
		}
		return []netip.Addr{address}, nil
	}

	canonicalHost, err := idna.Lookup.ToASCII(hostname)
	if err != nil {
		return nil, fmt.Errorf("publicnet: normalize target host: %w", err)
	}
	canonicalHost = strings.TrimSuffix(strings.ToLower(canonicalHost), ".")
	if canonicalHost == "" || strings.Contains(canonicalHost, "%") {
		return nil, errors.New("publicnet: target host is invalid")
	}
	if !policy.allowPrivate && (canonicalHost == "localhost" || strings.HasSuffix(canonicalHost, ".localhost")) {
		return nil, fmt.Errorf("%w: localhost is not allowed", ErrNotPublic)
	}

	resolved, err := policy.resolver.LookupNetIP(ctx, "ip", canonicalHost)
	if err != nil {
		return nil, fmt.Errorf("publicnet: resolve target host %q: %w", canonicalHost, err)
	}
	addresses := make([]netip.Addr, 0, len(resolved))
	seen := make(map[netip.Addr]struct{}, len(resolved))
	for _, value := range resolved {
		if !value.IsValid() || value.Zone() != "" {
			return nil, fmt.Errorf("publicnet: resolver returned an invalid address for %q", canonicalHost)
		}
		address := value.Unmap()
		if !policy.allowPrivate && !IsPublicAddress(address) {
			return nil, fmt.Errorf("%w: host %q resolves to private, local, or reserved address %s", ErrNotPublic, canonicalHost, address)
		}
		if _, duplicate := seen[address]; duplicate {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("publicnet: target host %q resolved to no addresses", canonicalHost)
	}
	return addresses, nil
}

// DialContext validates a TCP target, freshly resolves its host, and dials
// only validated literal IP candidates. The original hostname is never passed
// to the underlying dialer.
func (policy *Policy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("publicnet: network %q is not supported", network)
	}
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("publicnet: parse dial address: %w", err)
	}
	if rawPort == "" || strings.Trim(rawPort, "0123456789") != "" {
		return nil, errors.New("publicnet: target port must be between 1 and 65535")
	}
	portNumber, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || portNumber == 0 {
		return nil, errors.New("publicnet: target port must be between 1 and 65535")
	}
	port := strconv.FormatUint(portNumber, 10)

	addresses, err := policy.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var failures []error
	for _, candidate := range addresses {
		connection, dialErr := policy.dial(ctx, network, net.JoinHostPort(candidate.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		failures = append(failures, dialErr)
		if ctx.Err() != nil {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		failures = append(failures, err)
	}
	return nil, fmt.Errorf("publicnet: dial %s: %w", address, errors.Join(failures...))
}
