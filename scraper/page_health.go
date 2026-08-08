package scraper

import (
	"sync"
	"time"

	"github.com/go-rod/rod"
)

const (
	defaultMaxPageUses            = 50
	defaultMaxConsecutiveFailures = 3
	defaultMaxPageAge             = 50 * time.Minute
)

type pageRetirementPolicy struct {
	maxUses                int
	maxConsecutiveFailures int
	maxAge                 time.Duration
}

func defaultPageRetirementPolicy() pageRetirementPolicy {
	return pageRetirementPolicy{
		maxUses:                defaultMaxPageUses,
		maxConsecutiveFailures: defaultMaxConsecutiveFailures,
		maxAge:                 defaultMaxPageAge,
	}
}

type pageHealthState struct {
	mu                  sync.Mutex
	createdAt           time.Time
	uses                int
	consecutiveFailures int
}

func newPageHealthState(now time.Time) *pageHealthState {
	return &pageHealthState{createdAt: now}
}

func (state *pageHealthState) observe(now time.Time, succeeded bool, policy pageRetirementPolicy) bool {
	state.mu.Lock()
	defer state.mu.Unlock()

	state.uses++
	if succeeded {
		state.consecutiveFailures = 0
	} else {
		state.consecutiveFailures++
	}

	return policy.maxUses > 0 && state.uses >= policy.maxUses ||
		policy.maxConsecutiveFailures > 0 && state.consecutiveFailures >= policy.maxConsecutiveFailures ||
		policy.maxAge > 0 && now.Sub(state.createdAt) >= policy.maxAge
}

func (s *Scraper) observePageUse(page *rod.Page, succeeded bool) bool {
	now := time.Now()
	stateValue, _ := s.pageHealth.LoadOrStore(page, newPageHealthState(now))
	return stateValue.(*pageHealthState).observe(now, succeeded, s.pagePolicy)
}

func (s *Scraper) trackPage(page *rod.Page) {
	s.pageHealth.LoadOrStore(page, newPageHealthState(time.Now()))
}

func (s *Scraper) retirePage(page *rod.Page) {
	s.pageHealth.Delete(page)
	_ = page.Close()
	s.retiredPages.Add(1)
	// A nil token preserves the fixed-capacity semaphore and causes the next
	// borrower to create a fresh tab instead of reusing the retired page.
	s.pagePool.Put(nil)
}
