package main

import (
	"context"
	"errors"
	"sync"

	answerdomain "github.com/use-agent/purify/answer"
	"github.com/use-agent/purify/api/handler"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
	verifydomain "github.com/use-agent/purify/verify"
	watchdomain "github.com/use-agent/purify/watch"
)

var errManagedWatchConfigInvalid = errors.New("managed watch configuration is invalid")

// watchVerificationSubmitter adapts verify.Service, whose recorder parameter
// is interface-typed, to the watch scheduler's concrete recorder dependency.
type watchVerificationSubmitter struct {
	service *verifydomain.Service
}

func (submitter watchVerificationSubmitter) VerifyWithRecorder(
	ctx context.Context,
	request models.VerifyRequest,
	recorder *watchdomain.VerificationRecorder,
) (*models.VerifyResponse, error) {
	return submitter.service.VerifyWithRecorder(ctx, request, recorder)
}

// managedWatchRuntime owns the durable watch store and the one scheduler
// goroutine that drives due watches through verification.
type managedWatchRuntime struct {
	store     *watchdomain.Store
	scheduler *watchdomain.Scheduler
	closeOnce sync.Once
	closeErr  error
}

// newManagedWatchRuntime follows the verification and answer capabilities:
// a binary that cannot revisit evidence or compose fresh beliefs must not
// accept watches it can never check. Both dependencies present means the
// store and scheduler start together.
func newManagedWatchRuntime(
	parent context.Context,
	durable *ledger.Store,
	verifyService *verifydomain.Service,
	answerService *answerdomain.Service,
) (*managedWatchRuntime, error) {
	if verifyService == nil || answerService == nil {
		return nil, nil
	}
	if parent == nil || durable == nil {
		return nil, errManagedWatchConfigInvalid
	}
	store, err := watchdomain.NewStore(durable)
	if err != nil {
		return nil, errManagedWatchConfigInvalid
	}
	scheduler, err := watchdomain.NewScheduler(
		parent,
		store,
		watchVerificationSubmitter{service: verifyService},
		answerService,
		watchdomain.SchedulerOptions{},
	)
	if err != nil {
		return nil, errManagedWatchConfigInvalid
	}
	return &managedWatchRuntime{store: store, scheduler: scheduler}, nil
}

// Close stops the scheduler exactly once. It is safe on a nil runtime and
// from concurrent shutdown paths.
func (runtime *managedWatchRuntime) Close() error {
	if runtime == nil {
		return nil
	}
	runtime.closeOnce.Do(func() {
		if runtime.scheduler != nil {
			runtime.closeErr = runtime.scheduler.Close()
		}
	})
	return runtime.closeErr
}

// managedWatchHandlerService avoids storing a typed nil in the transport
// interface when the watch capability is disabled.
func managedWatchHandlerService(runtime *managedWatchRuntime) handler.WatchService {
	if runtime == nil || runtime.store == nil {
		return nil
	}
	return runtime.store
}
