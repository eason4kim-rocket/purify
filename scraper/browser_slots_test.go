package scraper

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/config"
)

func TestNewScraperRejectsNonPositiveBrowserCapacityBeforeLaunch(t *testing.T) {
	if scraper, err := NewScraper(config.BrowserConfig{MaxPages: 0}, config.ScraperConfig{}); err == nil || scraper != nil {
		t.Fatalf("NewScraper(MaxPages=0) = (%#v, %v)", scraper, err)
	}
}

func TestBrowserSlotGateEnforcesNAndNPlusOne(t *testing.T) {
	gate := newBrowserSlotGate(2)
	first, err := gate.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := gate.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	thirdAcquired := make(chan func(), 1)
	thirdErr := make(chan error, 1)
	go func() {
		release, acquireErr := gate.acquire(context.Background())
		if acquireErr != nil {
			thirdErr <- acquireErr
			return
		}
		thirdAcquired <- release
	}()
	select {
	case <-thirdAcquired:
		t.Fatal("N+1 browser acquisition bypassed the gate")
	case err := <-thirdErr:
		t.Fatalf("N+1 browser acquisition failed early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	first()
	select {
	case release := <-thirdAcquired:
		release()
	case err := <-thirdErr:
		t.Fatalf("N+1 browser acquisition failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("N+1 browser acquisition did not resume after release")
	}
	second()
	gate.closeAndWait()
}

func TestBrowserSlotGateCancellationDoesNotLeakCapacity(t *testing.T) {
	gate := newBrowserSlotGate(1)
	release, err := gate.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithCancel(context.Background())
	waitErr := make(chan error, 1)
	go func() {
		_, acquireErr := gate.acquire(waitCtx)
		waitErr <- acquireErr
	}()
	cancel()
	select {
	case err := <-waitErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled acquisition error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled browser acquisition remained blocked")
	}

	release()
	reused, err := gate.acquire(context.Background())
	if err != nil {
		t.Fatalf("capacity leaked after canceled waiter: %v", err)
	}
	reused()
	gate.closeAndWait()
}

func TestBrowserSlotGateCloseUnblocksWaitersAndWaitsForUsers(t *testing.T) {
	gate := newBrowserSlotGate(1)
	release, err := gate.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	waitErr := make(chan error, 1)
	go func() {
		_, acquireErr := gate.acquire(context.Background())
		waitErr <- acquireErr
	}()
	closed := make(chan struct{})
	go func() {
		gate.closeAndWait()
		close(closed)
	}()

	select {
	case err := <-waitErr:
		if !errors.Is(err, errBrowserSlotsClosed) {
			t.Fatalf("queued waiter error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock queued waiter")
	}
	select {
	case <-closed:
		t.Fatal("close returned while an acquired browser slot remained active")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not return after active slot release")
	}
	if _, err := gate.acquire(context.Background()); !errors.Is(err, errBrowserSlotsClosed) {
		t.Fatalf("post-close acquisition error = %v", err)
	}
	gate.closeAndWait() // idempotent
}

func TestBrowserSlotGateConcurrentCancelReleaseAndClose(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		gate := newBrowserSlotGate(3)
		var callers sync.WaitGroup
		for index := 0; index < 24; index++ {
			callers.Add(1)
			go func(cancelImmediately bool) {
				defer callers.Done()
				ctx, cancel := context.WithCancel(context.Background())
				if cancelImmediately {
					cancel()
				} else {
					defer cancel()
				}
				release, acquireErr := gate.acquire(ctx)
				if acquireErr == nil {
					release()
					release() // release is intentionally idempotent
				}
			}(index%3 == 0)
		}
		gate.closeAndWait()
		callers.Wait()
	}
}

func TestBrowserSlotLeaseTransfersToAsyncContextCleanup(t *testing.T) {
	gate := newBrowserSlotGate(1)
	release, err := gate.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cleanupDone := make(chan struct{})
	if !releaseBrowserSlotAfter(cleanupDone, release) {
		t.Fatal("open cleanup channel did not take ownership of the browser slot")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := gate.acquire(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("N+1 acquisition before cleanup error = %v", err)
	}

	closed := make(chan struct{})
	go func() {
		gate.closeAndWait()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("gate closed before transferred context cleanup completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(cleanupDone)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("gate did not close after transferred context cleanup completed")
	}
}
