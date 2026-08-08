package scraper

import (
	"testing"
	"time"
)

func TestPageHealthRetirementPolicy(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	policy := pageRetirementPolicy{
		maxUses:                3,
		maxConsecutiveFailures: 2,
		maxAge:                 time.Hour,
	}

	t.Run("retires at use limit", func(t *testing.T) {
		state := newPageHealthState(now)
		if state.observe(now, true, policy) || state.observe(now, true, policy) {
			t.Fatal("page retired before reaching use limit")
		}
		if !state.observe(now, true, policy) {
			t.Fatal("page did not retire at use limit")
		}
	})

	t.Run("retires after consecutive failures", func(t *testing.T) {
		state := newPageHealthState(now)
		if state.observe(now, false, policy) {
			t.Fatal("page retired after one failure")
		}
		if !state.observe(now, false, policy) {
			t.Fatal("page did not retire after consecutive failures")
		}
	})

	t.Run("success resets failure streak", func(t *testing.T) {
		state := newPageHealthState(now)
		resetPolicy := policy
		resetPolicy.maxUses = 10
		if state.observe(now, false, resetPolicy) || state.observe(now, true, resetPolicy) {
			t.Fatal("page retired before reset assertion")
		}
		if state.observe(now, false, resetPolicy) {
			t.Fatal("one failure after a success should not retain the prior failure streak")
		}
		if !state.observe(now, false, resetPolicy) {
			t.Fatal("page did not retire after a new consecutive failure streak")
		}
	})

	t.Run("retires at age limit", func(t *testing.T) {
		state := newPageHealthState(now)
		if !state.observe(now.Add(time.Hour), true, policy) {
			t.Fatal("page did not retire at age limit")
		}
	})
}
