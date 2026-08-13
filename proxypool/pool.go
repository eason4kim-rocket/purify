// Package proxypool hands out egress proxy URLs in round-robin order so fetch
// traffic can rotate across several exits instead of pinning one IP.
//
// It holds only opaque proxy URL strings and never dials: the fetch engines
// keep ownership of connecting, auth, and the public-only egress policy. A pool
// with no entries returns the empty string, which every engine already treats
// as "dial direct", so callers can wire a pool unconditionally.
package proxypool

import (
	"strings"
	"sync/atomic"
)

// Pool selects one proxy URL per call. It is safe for concurrent use.
type Pool struct {
	proxies []string
	cursor  atomic.Uint64
}

// New builds a pool from the given proxy URLs, dropping blank entries. Order is
// preserved so a deployment can put its most trusted exit first.
func New(proxies []string) *Pool {
	cleaned := make([]string, 0, len(proxies))
	for _, proxy := range proxies {
		if trimmed := strings.TrimSpace(proxy); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	return &Pool{proxies: cleaned}
}

// Len reports how many usable proxies the pool holds.
func (p *Pool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.proxies)
}

// Next returns the next proxy URL in round-robin order, or the empty string
// when the pool is empty. A single-entry pool always returns that entry without
// touching the cursor, keeping the common case allocation- and contention-free.
func (p *Pool) Next() string {
	if p == nil || len(p.proxies) == 0 {
		return ""
	}
	if len(p.proxies) == 1 {
		return p.proxies[0]
	}
	index := p.cursor.Add(1) - 1
	return p.proxies[index%uint64(len(p.proxies))]
}
