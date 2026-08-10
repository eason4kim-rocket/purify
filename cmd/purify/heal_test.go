package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"

	compilerdomain "github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/snapshot"
)

func TestManagedHealRuntimeFollowsSnapshotCapabilityNotCompilerEnablement(t *testing.T) {
	durable, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	registry, err := compilerdomain.NewStore(durable)
	if err != nil {
		t.Fatal(err)
	}

	runtime, err := newManagedHealRuntime(context.Background(), config.HealConfig{}, registry, nil)
	if err != nil || runtime != nil {
		t.Fatalf("snapshots-off empty runtime = %#v, %v", runtime, err)
	}
	configured := config.HealConfig{WebhookURL: "https://hooks.example.com/heal"}
	runtime, err = newManagedHealRuntime(context.Background(), configured, registry, nil)
	if !errors.Is(err, config.ErrInvalidHealConfig) || runtime != nil {
		t.Fatalf("snapshots-off configured runtime = %#v, %v", runtime, err)
	}

	snapshots, err := snapshot.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(snapshots.Close)
	runtime, err = newManagedHealRuntime(context.Background(), configured, registry, snapshots)
	if err != nil || runtime == nil || runtime.worker == nil {
		t.Fatalf("snapshots-on runtime = %#v, %v", runtime, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("idempotent Close = %v", err)
	}
}

func TestManagedHealRuntimeForwardsScheduleAndClosesOnce(t *testing.T) {
	stub := &managedHealWorkerStub{
		schedule: compilerdomain.HealSchedule{
			ExtractorID: "00000000-0000-4000-8000-000000000001",
			HealRunID:   "00000000-0000-4000-8000-000000000002",
		},
	}
	runtime := &managedHealRuntime{worker: stub}
	schedule, err := runtime.ScheduleExtractor(context.Background(), stub.schedule.ExtractorID)
	if err != nil || schedule != stub.schedule || stub.scheduleCalls != 1 {
		t.Fatalf("ScheduleExtractor = %+v, %v calls=%d", schedule, err, stub.scheduleCalls)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil || stub.closeCalls != 1 {
		t.Fatalf("idempotent Close = %v calls=%d", err, stub.closeCalls)
	}
	if _, err := (*managedHealRuntime)(nil).ScheduleExtractor(
		context.Background(), stub.schedule.ExtractorID,
	); !errors.Is(err, compilerdomain.ErrHealWorkerClosed) {
		t.Fatalf("nil runtime schedule = %v", err)
	}
}

func TestManagedBackgroundLifecycleClosesInDependencyOrderOnce(t *testing.T) {
	events := make([]string, 0, 5)
	compilerFailure := errors.New("compiler close failure")
	lifecycle := &managedBackgroundLifecycle{
		watch:    &orderedManagedCloser{name: "watch", events: &events},
		compiler: &orderedManagedCloser{name: "compiler", events: &events, err: compilerFailure},
		heal:     &orderedManagedCloser{name: "heal", events: &events},
		outbox:   &orderedManagedCloser{name: "outbox", events: &events},
		relay:    &orderedManagedCloser{name: "relay", events: &events},
		closeOutbox: func() {
			events = append(events, "outbox-http")
		},
	}
	for index := 0; index < 2; index++ {
		if err := lifecycle.Close(); !errors.Is(err, compilerFailure) {
			t.Fatalf("Close %d = %v", index, err)
		}
	}
	if got := strings.Join(events, ","); got != "watch,compiler,heal,outbox,relay,outbox-http" {
		t.Fatalf("close order = %s", got)
	}
	if err := (*managedBackgroundLifecycle)(nil).Close(); err != nil {
		t.Fatalf("nil lifecycle Close = %v", err)
	}
}

func TestManagedSafeRelayFollowsSnapshotCapability(t *testing.T) {
	if relay, safeURL, err := newManagedSafeRelay(nil, nil); err != nil || relay != nil || safeURL != "" {
		t.Fatalf("snapshots-off relay = %#v/%q, %v", relay, safeURL, err)
	}
	snapshots, err := snapshot.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(snapshots.Close)
	if relay, safeURL, err := newManagedSafeRelay(snapshots, nil); !errors.Is(err, errManagedSafeRelayUnavailable) ||
		relay != nil || safeURL != "" {
		t.Fatalf("nil-policy relay = %#v/%q, %v", relay, safeURL, err)
	}
	relay, safeURL, err := newManagedSafeRelay(snapshots, publicnet.NewPolicy(publicnet.Options{}))
	if err != nil || relay == nil {
		t.Fatalf("snapshots-on relay = %#v/%q, %v", relay, safeURL, err)
	}
	parsed, err := url.Parse(safeURL)
	if err != nil || parsed.Scheme != "socks5" || parsed.Port() == "" ||
		net.ParseIP(parsed.Hostname()) == nil || !net.ParseIP(parsed.Hostname()).IsLoopback() {
		t.Fatalf("safe relay URL = %q, %v", safeURL, err)
	}
	if err := relay.Close(); err != nil {
		t.Fatal(err)
	}
	if err := relay.Close(); err != nil {
		t.Fatalf("idempotent relay Close = %v", err)
	}
}

type managedHealWorkerStub struct {
	schedule      compilerdomain.HealSchedule
	err           error
	scheduleCalls int
	closeCalls    int
}

func (stub *managedHealWorkerStub) ScheduleExtractor(
	_ context.Context,
	_ string,
) (compilerdomain.HealSchedule, error) {
	stub.scheduleCalls++
	return stub.schedule, stub.err
}

func (stub *managedHealWorkerStub) Close() error {
	stub.closeCalls++
	return nil
}

type orderedManagedCloser struct {
	name   string
	events *[]string
	err    error
}

func (closer *orderedManagedCloser) Close() error {
	*closer.events = append(*closer.events, closer.name)
	return closer.err
}
