package indexer

import (
	"bufio"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultCrawlDelay = time.Second

type robotsGroup struct {
	agents     []string
	allows     []string
	disallows  []string
	crawlDelay time.Duration
	hasDelay   bool
}

// Robots is a minimal robots.txt policy for one host.
type Robots struct {
	groups []robotsGroup
}

// ParseRobots reads User-agent / Allow / Disallow / Crawl-delay groups.
func ParseRobots(body io.Reader) Robots {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
	var groups []robotsGroup
	var current robotsGroup
	inGroup := false
	flush := func() {
		if !inGroup {
			return
		}
		groups = append(groups, current)
		current = robotsGroup{}
		inGroup = false
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "user-agent":
			if inGroup && (len(current.allows) > 0 || len(current.disallows) > 0 || current.hasDelay) {
				flush()
			}
			inGroup = true
			current.agents = append(current.agents, strings.ToLower(value))
		case "disallow":
			if inGroup {
				current.disallows = append(current.disallows, value)
			}
		case "allow":
			if inGroup {
				current.allows = append(current.allows, value)
			}
		case "crawl-delay":
			if inGroup {
				seconds, err := strconv.ParseFloat(value, 64)
				if err == nil && seconds >= 0 {
					current.crawlDelay = time.Duration(seconds * float64(time.Second))
					current.hasDelay = true
				}
			}
		}
	}
	flush()
	return Robots{groups: groups}
}

// Allowed reports whether path is crawlable for the given user-agent.
func (r Robots) Allowed(userAgent, rawURL string) bool {
	path := robotsPath(rawURL)
	group, ok := r.matchGroup(userAgent)
	if !ok {
		return true
	}
	allowLen, denyLen := -1, -1
	for _, rule := range group.allows {
		if robotsMatch(rule, path) && len(rule) > allowLen {
			allowLen = len(rule)
		}
	}
	for _, rule := range group.disallows {
		if robotsMatch(rule, path) && len(rule) > denyLen {
			denyLen = len(rule)
		}
	}
	if allowLen < 0 && denyLen < 0 {
		return true
	}
	return allowLen >= denyLen
}

// CrawlDelay returns the matching group's delay, or the default 1s.
func (r Robots) CrawlDelay(userAgent string) time.Duration {
	group, ok := r.matchGroup(userAgent)
	if !ok || !group.hasDelay {
		return defaultCrawlDelay
	}
	return group.crawlDelay
}

func (r Robots) matchGroup(userAgent string) (robotsGroup, bool) {
	agent := strings.ToLower(userAgent)
	var wildcard *robotsGroup
	for i := range r.groups {
		group := &r.groups[i]
		for _, name := range group.agents {
			if name == "*" {
				wildcard = group
				continue
			}
			if strings.Contains(agent, name) {
				return *group, true
			}
		}
	}
	if wildcard != nil {
		return *wildcard, true
	}
	return robotsGroup{}, false
}

func robotsPath(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Path == "" {
		return "/"
	}
	if parsed.RawQuery != "" {
		return parsed.Path + "?" + parsed.RawQuery
	}
	return parsed.Path
}

func robotsMatch(rule, path string) bool {
	if rule == "" {
		return false
	}
	if strings.HasSuffix(rule, "$") {
		return path == strings.TrimSuffix(rule, "$")
	}
	return strings.HasPrefix(path, rule)
}
