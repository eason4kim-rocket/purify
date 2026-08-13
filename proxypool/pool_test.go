package proxypool

import (
	"sync"
	"testing"
)

func TestNewDropsBlankEntriesAndPreservesOrder(t *testing.T) {
	pool := New([]string{" http://a ", "", "   ", "socks5://b"})
	if pool.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", pool.Len())
	}
	if got := pool.Next(); got != "http://a" {
		t.Fatalf("first Next() = %q, want trimmed http://a", got)
	}
}

func TestNextRoundRobins(t *testing.T) {
	pool := New([]string{"http://a", "http://b", "http://c"})
	want := []string{"http://a", "http://b", "http://c", "http://a", "http://b"}
	for i, expected := range want {
		if got := pool.Next(); got != expected {
			t.Fatalf("Next() call %d = %q, want %q", i, got, expected)
		}
	}
}

func TestSingleEntryAlwaysReturnsSameProxy(t *testing.T) {
	pool := New([]string{"http://only"})
	for range 3 {
		if got := pool.Next(); got != "http://only" {
			t.Fatalf("Next() = %q, want http://only", got)
		}
	}
}

func TestEmptyAndNilPoolReturnEmptyString(t *testing.T) {
	if got := New(nil).Next(); got != "" {
		t.Fatalf("empty pool Next() = %q, want empty string", got)
	}
	if got := New([]string{"", "  "}).Next(); got != "" {
		t.Fatalf("blank-only pool Next() = %q, want empty string", got)
	}
	var nilPool *Pool
	if got := nilPool.Next(); got != "" {
		t.Fatalf("nil pool Next() = %q, want empty string", got)
	}
	if nilPool.Len() != 0 {
		t.Fatalf("nil pool Len() = %d, want 0", nilPool.Len())
	}
}

// TestNextIsConcurrencySafe covers the round-robin cursor under the race
// detector: many goroutines calling Next must never index out of range or race
// on the cursor.
func TestNextIsConcurrencySafe(t *testing.T) {
	pool := New([]string{"http://a", "http://b", "http://c", "http://d"})
	valid := map[string]bool{"http://a": true, "http://b": true, "http://c": true, "http://d": true}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if got := pool.Next(); !valid[got] {
					t.Errorf("Next() returned unknown proxy %q", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
