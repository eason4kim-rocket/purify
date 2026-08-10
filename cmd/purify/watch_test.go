package main

import (
	"context"
	"errors"
	"testing"

	answerdomain "github.com/use-agent/purify/answer"
	verifydomain "github.com/use-agent/purify/verify"
)

func TestManagedWatchRuntimeFollowsVerifyAndAnswerCapabilities(t *testing.T) {
	if runtime, err := newManagedWatchRuntime(context.Background(), nil, nil, nil); runtime != nil || err != nil {
		t.Fatalf("fully disabled runtime = (%#v,%v)", runtime, err)
	}
	if runtime, err := newManagedWatchRuntime(context.Background(), nil, &verifydomain.Service{}, nil); runtime != nil || err != nil {
		t.Fatalf("answer-less runtime = (%#v,%v)", runtime, err)
	}
	if runtime, err := newManagedWatchRuntime(context.Background(), nil, nil, &answerdomain.Service{}); runtime != nil || err != nil {
		t.Fatalf("verify-less runtime = (%#v,%v)", runtime, err)
	}

	// Both capabilities present with a missing durable store must fail loudly
	// instead of silently disabling the advertised capability.
	runtime, err := newManagedWatchRuntime(context.Background(), nil, &verifydomain.Service{}, &answerdomain.Service{})
	if runtime != nil || !errors.Is(err, errManagedWatchConfigInvalid) {
		t.Fatalf("store-less runtime = (%#v,%v)", runtime, err)
	}
	runtime, err = newManagedWatchRuntime(nil, nil, &verifydomain.Service{}, &answerdomain.Service{})
	if runtime != nil || !errors.Is(err, errManagedWatchConfigInvalid) {
		t.Fatalf("context-less runtime = (%#v,%v)", runtime, err)
	}
}

func TestManagedWatchHandlerServiceAndCloseAreNilSafe(t *testing.T) {
	if service := managedWatchHandlerService(nil); service != nil {
		t.Fatalf("nil runtime handler service = %#v", service)
	}
	if service := managedWatchHandlerService(&managedWatchRuntime{}); service != nil {
		t.Fatalf("store-less runtime handler service = %#v", service)
	}
	if err := (*managedWatchRuntime)(nil).Close(); err != nil {
		t.Fatalf("nil runtime Close() = %v", err)
	}
	if err := (&managedWatchRuntime{}).Close(); err != nil {
		t.Fatalf("empty runtime Close() = %v", err)
	}
}
