package scraper

import (
	"context"
	"errors"
	"sync"
)

var errBrowserSlotsClosed = errors.New("scraper: browser is closing")

// browserSlotGate is the process-wide browser concurrency boundary. Unlike the
// reusable Rod page pool, it also covers request-isolated BrowserContexts and
// caller-provided CDP connections.
//
// A waiter registers under mu before blocking. closeAndWait marks the gate
// closed under the same mutex, so sync.WaitGroup.Add can never race with Wait.
type browserSlotGate struct {
	slots   chan struct{}
	closed  chan struct{}
	mu      sync.Mutex
	closing bool
	users   sync.WaitGroup
}

func newBrowserSlotGate(capacity int) *browserSlotGate {
	return &browserSlotGate{
		slots:  make(chan struct{}, capacity),
		closed: make(chan struct{}),
	}
}

func (gate *browserSlotGate) acquire(ctx context.Context) (func(), error) {
	if gate == nil {
		return func() {}, errBrowserSlotsClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}

	gate.mu.Lock()
	if gate.closing {
		gate.mu.Unlock()
		return func() {}, errBrowserSlotsClosed
	}
	gate.users.Add(1)
	gate.mu.Unlock()

	select {
	case gate.slots <- struct{}{}:
		// A close or cancellation can become ready at the same instant as the
		// slot send. Re-check both before making the acquisition visible.
		gate.mu.Lock()
		closing := gate.closing
		gate.mu.Unlock()
		if closing {
			<-gate.slots
			gate.users.Done()
			return func() {}, errBrowserSlotsClosed
		}
		if err := ctx.Err(); err != nil {
			<-gate.slots
			gate.users.Done()
			return func() {}, err
		}
		var once sync.Once
		return func() {
			once.Do(func() {
				<-gate.slots
				gate.users.Done()
			})
		}, nil
	case <-ctx.Done():
		gate.users.Done()
		return func() {}, ctx.Err()
	case <-gate.closed:
		gate.users.Done()
		return func() {}, errBrowserSlotsClosed
	}
}

func (gate *browserSlotGate) closeAndWait() {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	if !gate.closing {
		gate.closing = true
		close(gate.closed)
	}
	gate.mu.Unlock()
	gate.users.Wait()
}

// releaseBrowserSlotAfter transfers an acquired slot to asynchronous browser
// cleanup. It returns true only when release will run from the cleanup waiter.
func releaseBrowserSlotAfter(cleanupDone <-chan struct{}, release func()) bool {
	if cleanupDone == nil || release == nil {
		return false
	}
	select {
	case <-cleanupDone:
		return false
	default:
		go func() {
			<-cleanupDone
			release()
		}()
		return true
	}
}
