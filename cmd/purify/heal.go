package main

import (
	"context"
	"errors"
	"sync"

	compilerdomain "github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/proxy"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/snapshot"
)

var errManagedSafeRelayUnavailable = errors.New("managed safe relay is unavailable")

type managedHealWorker interface {
	ScheduleExtractor(context.Context, string) (compilerdomain.HealSchedule, error)
	Close() error
}

type managedBackgroundCloser interface {
	Close() error
}

// managedBackgroundLifecycle establishes the explicit shutdown dependency
// order after the HTTP server has drained. Earlier per-resource defers remain
// safe initialization-failure fallbacks because every component is idempotent.
type managedBackgroundLifecycle struct {
	compiler    managedBackgroundCloser
	heal        managedBackgroundCloser
	outbox      managedBackgroundCloser
	relay       managedBackgroundCloser
	closeOutbox func()

	closeOnce sync.Once
	closeErr  error
}

func (lifecycle *managedBackgroundLifecycle) Close() error {
	if lifecycle == nil {
		return nil
	}
	lifecycle.closeOnce.Do(func() {
		var failures []error
		for _, closer := range []managedBackgroundCloser{
			lifecycle.compiler,
			lifecycle.heal,
			lifecycle.outbox,
			lifecycle.relay,
		} {
			if closer != nil {
				failures = append(failures, closer.Close())
			}
		}
		if lifecycle.closeOutbox != nil {
			lifecycle.closeOutbox()
		}
		lifecycle.closeErr = errors.Join(failures...)
	})
	return lifecycle.closeErr
}

// managedHealRuntime owns production self-heal polling independently of the
// optional managed compiler synthesis runtime.
type managedHealRuntime struct {
	worker    managedHealWorker
	closeOnce sync.Once
	closeErr  error
}

func (runtime *managedHealRuntime) ScheduleExtractor(
	ctx context.Context,
	extractorID string,
) (compilerdomain.HealSchedule, error) {
	if runtime == nil || runtime.worker == nil {
		return compilerdomain.HealSchedule{}, compilerdomain.ErrHealWorkerClosed
	}
	return runtime.worker.ScheduleExtractor(ctx, extractorID)
}

func (runtime *managedHealRuntime) Close() error {
	if runtime == nil {
		return nil
	}
	runtime.closeOnce.Do(func() {
		if runtime.worker != nil {
			runtime.closeErr = runtime.worker.Close()
		}
	})
	return runtime.closeErr
}

func newManagedHealRuntime(
	parent context.Context,
	cfg config.HealConfig,
	registry *compilerdomain.Store,
	snapshots *snapshot.Store,
) (*managedHealRuntime, error) {
	if err := config.ValidateHealConfig(cfg, snapshots != nil); err != nil {
		return nil, err
	}
	if snapshots == nil {
		return nil, nil
	}
	if parent == nil || registry == nil {
		return nil, compilerdomain.ErrInvalidHealWorker
	}
	options := make([]compilerdomain.HealerOption, 0, 1)
	if cfg.WebhookURL != "" {
		options = append(options, compilerdomain.WithHealWebhook(cfg.WebhookURL, cfg.WebhookSecret))
	}
	healer, err := compilerdomain.NewHealer(registry, snapshots, options...)
	if err != nil {
		return nil, err
	}
	worker, err := compilerdomain.NewHealWorker(parent, healer, compilerdomain.HealWorkerOptions{})
	if err != nil {
		return nil, err
	}
	return &managedHealRuntime{worker: worker}, nil
}

// newManagedSafeRelay creates the one process-owned SOCKS5 boundary shared by
// verification revisit and multi-source extraction whenever snapshots exist.
func newManagedSafeRelay(
	snapshots *snapshot.Store,
	policy *publicnet.Policy,
) (*proxy.Relay, string, error) {
	if snapshots == nil {
		return nil, "", nil
	}
	if policy == nil {
		return nil, "", errManagedSafeRelayUnavailable
	}
	relay, err := proxy.StartDirectRelay(policy.DialContext)
	if err != nil {
		return nil, "", errManagedSafeRelayUnavailable
	}
	return relay, "socks5://" + relay.Addr(), nil
}
