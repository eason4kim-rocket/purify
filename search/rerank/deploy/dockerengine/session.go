package dockerengine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/use-agent/purify/search/rerank"
	"github.com/use-agent/purify/search/rerank/deploy"
)

// ErrSessionUnavailable is the stable public failure for the recording-only
// local session. Dependency, Docker, path, key, and model details are never
// wrapped into it.
var ErrSessionUnavailable = errors.New("rerank docker engine: reference session unavailable")

const sessionCleanupTimeout = 15 * time.Second

type sessionRecorder interface {
	Record(context.Context, rerank.ScoreRequest) (rerank.ReferenceRecording, error)
	Close()
}

// ReferenceSession is an unforgeable, live recording handle. Its endpoint,
// API key, Docker engine, ownership state, and recorder remain private. It is
// deliberately not a rerank.Scorer and cannot be installed in Search.
type ReferenceSession struct {
	mu               sync.Mutex
	engine           Engine
	plan             *referencePlan
	owner            ownership
	recorder         sessionRecorder
	evidence         Evidence
	lifetime         context.Context
	cancel           context.CancelFunc
	done             chan struct{}
	live             bool
	authorityOnce    sync.Once
	shutdownOnce     sync.Once
	cleanupMu        sync.Mutex
	cleanupDone      bool
	containerCleaned bool
	hostCleaned      bool
	engineClosed     bool
	cleanupErr       error
	cleanupHost      func() error
	cleanupOwned     atomic.Bool
	cancelEvents     context.CancelFunc
	eventGuard       *sessionEventGuard
	verifyRunning    func(context.Context) (RunningInspection, error)
	anchorPID        int
}

type sessionEventGuard struct {
	mu       sync.Mutex
	final    sessionEventState
	commands chan sessionEventCommand
	done     chan struct{}
}

type sessionEventCommand struct {
	kind     string
	session  *ReferenceSession
	response chan sessionEventState
}

type sessionEventState struct {
	published bool
	foreign   bool
	fatal     bool
}

type sessionDependencies struct {
	engine                   Engine
	controller               ControllerInspection
	paths                    HostPaths
	runID                    string
	apiKey                   string
	prepareHost              func(context.Context, HostPaths) error
	verifyHostArtifacts      func(context.Context) error
	verifyContainerArtifacts func(context.Context, Engine, string) error
	readProcess              func(context.Context, int) (ProcessInspection, error)
	inspectSocket            func(context.Context, string) (SocketInspection, error)
	probeUnauthorized        func(context.Context, string) error
	newRecorder              func(string, string) (sessionRecorder, error)
	probeAuthenticated       func(context.Context, sessionRecorder) error
	cleanupHost              func() error
}

// startReferenceSession is intentionally package-private. The only eventual
// production wrapper supplies the fixed local Moby adapter and host-owned
// hooks; tests inject fakes without exposing Engine or credentials to callers.
func startReferenceSession(ctx context.Context, dependencies sessionDependencies) (*ReferenceSession, error) {
	if ctx == nil || ctx.Err() != nil || !validSessionDependencies(dependencies) {
		if dependencies.engine != nil {
			_ = dependencies.engine.Close()
		}
		return nil, sessionStartError(ctx)
	}

	prepared := false
	var plan *referencePlan
	var owner ownership
	created := false
	cleanupOwned := true
	var cancelEvents context.CancelFunc
	var eventGuard *sessionEventGuard
	fail := func() (*ReferenceSession, error) {
		if eventGuard != nil {
			state := eventGuard.stop()
			if state.foreign {
				cleanupOwned = false
			}
			<-eventGuard.done
		}
		if cancelEvents != nil {
			cancelEvents()
		}
		cleanedContainer := !created
		if created && cleanupOwned {
			cleanupContext, cancel := context.WithTimeout(context.Background(), sessionCleanupTimeout)
			cleanedContainer = cleanupOwnedUntilSettled(cleanupContext, dependencies.engine, plan, owner) == nil
			cancel()
		}
		if prepared && cleanedContainer {
			_ = dependencies.cleanupHost()
		}
		_ = dependencies.engine.Close()
		return nil, sessionStartError(ctx)
	}
	var err error
	plan, err = buildReferencePlan(deploy.ReferenceDescriptor(), dependencies.paths, generatedInputs{
		RunID: dependencies.runID, APIKey: dependencies.apiKey,
	})
	if err != nil || ctx.Err() != nil {
		return fail()
	}

	daemon, err := dependencies.engine.InspectDaemon(ctx)
	if err != nil || ValidateAdmission(dependencies.controller, daemon) != nil || ctx.Err() != nil {
		return fail()
	}
	image, err := dependencies.engine.InspectImage(ctx, ReferenceImageReference)
	if err != nil || ValidateReferenceImage(image) != nil || ctx.Err() != nil {
		return fail()
	}
	if err := dependencies.prepareHost(ctx, dependencies.paths); err != nil || ctx.Err() != nil {
		return fail()
	}
	prepared = true
	result, err := dependencies.engine.Create(ctx, plan.cloneCreateSpec())
	if validLowerHex(result.ContainerID, 64) {
		owner = ownership{
			containerID: result.ContainerID, containerName: plan.create.Name,
			labels: ownershipLabels(plan.create.Labels),
		}
		created = true
	}
	if err != nil {
		return fail()
	}
	establishedOwner, err := plan.establishOwnership(result)
	if err != nil {
		return fail()
	}
	owner = establishedOwner
	inspection, err := dependencies.engine.Inspect(ctx, owner.containerID)
	if err == nil && validateOwnership(owner, inspection.ID, inspection.Labels) != nil {
		cleanupOwned = false
	}
	if err != nil || !cleanupOwned || validateCreated(plan, owner, inspection) != nil || ctx.Err() != nil {
		return fail()
	}
	eventCursor := time.Now().UnixNano()
	eventContext, cancelEventStream := context.WithCancel(context.Background())
	cancelEvents = cancelEventStream
	stopStartupCancellation := context.AfterFunc(ctx, cancelEventStream)
	events, eventErrors := dependencies.engine.Events(eventContext, owner.containerID, eventCursor)
	stopStartupCancellation()
	if events == nil || eventErrors == nil || ctx.Err() != nil {
		return fail()
	}
	// Docker flushes the event-stream response before its listener is fully
	// installed. Replayed, owner-bound archive events form a positive barrier:
	// Start is forbidden until both stopped-container reads are acknowledged.
	preStartArchives, err := dependencies.engine.ExpectReferenceArchiveEvents(ctx, owner.containerID, owner.labels, 2)
	if err != nil || preStartArchives == nil ||
		dependencies.verifyContainerArtifacts(ctx, dependencies.engine, owner.containerID) != nil || ctx.Err() != nil {
		return fail()
	}
	if err := awaitArchiveBarrier(ctx, preStartArchives, events, eventErrors, owner); err != nil {
		if errors.Is(err, ErrOwnershipLost) {
			cleanupOwned = false
		}
		return fail()
	}
	if err := dependencies.engine.Start(ctx, owner.containerID); err != nil {
		return fail()
	}
	if err := awaitStartEvent(ctx, events, eventErrors, owner); err != nil {
		if errors.Is(err, ErrOwnershipLost) {
			cleanupOwned = false
		}
		return fail()
	}
	eventGuard = newSessionEventGuard(eventContext, events, eventErrors, owner)

	firstRunning, err := inspectRunning(ctx, dependencies, plan, owner)
	if err != nil {
		if errors.Is(err, ErrOwnershipLost) {
			cleanupOwned = false
		}
		return fail()
	}
	if err := dependencies.probeUnauthorized(ctx, dependencies.paths.RunDir); err != nil || ctx.Err() != nil {
		return fail()
	}
	recorder, err := dependencies.newRecorder(dependencies.paths.RunDir, dependencies.apiKey)
	if err != nil || recorder == nil {
		return fail()
	}
	if err := dependencies.probeAuthenticated(ctx, recorder); err != nil || ctx.Err() != nil {
		recorder.Close()
		return fail()
	}
	// Recheck the host source, then the actual running mount view. The bounded
	// engine consumes exactly the two archive-path events generated by these
	// controller-owned reads; any unregistered archive event remains fatal.
	if err := dependencies.verifyHostArtifacts(ctx); err != nil || ctx.Err() != nil {
		recorder.Close()
		return fail()
	}
	archiveEvents, err := dependencies.engine.ExpectReferenceArchiveEvents(ctx, owner.containerID, owner.labels, 2)
	if err != nil || archiveEvents == nil ||
		dependencies.verifyContainerArtifacts(ctx, dependencies.engine, owner.containerID) != nil || ctx.Err() != nil {
		recorder.Close()
		return fail()
	}
	select {
	case <-archiveEvents:
	case <-eventGuard.done:
		// An event-stream failure or lifecycle/ownership drift revokes the
		// archive allowance. Never wait forever for acknowledgements that can
		// no longer arrive.
		recorder.Close()
		return fail()
	case <-ctx.Done():
		recorder.Close()
		return fail()
	}
	finalRunning, err := inspectRunning(ctx, dependencies, plan, owner)
	if err != nil || firstRunning.After.State.PID != finalRunning.Before.State.PID {
		if errors.Is(err, ErrOwnershipLost) {
			cleanupOwned = false
		}
		recorder.Close()
		return fail()
	}
	evidence, err := newRunningEvidence(plan, owner, image, finalRunning)
	if err != nil || ctx.Err() != nil {
		recorder.Close()
		return fail()
	}

	lifetime, cancelLifetime := context.WithCancel(context.Background())
	session := &ReferenceSession{
		engine: dependencies.engine, plan: plan, owner: owner, recorder: recorder,
		evidence: evidence, lifetime: lifetime, cancel: cancelLifetime, done: make(chan struct{}),
		live: true, cleanupHost: dependencies.cleanupHost, cancelEvents: cancelEvents, eventGuard: eventGuard,
		anchorPID: finalRunning.After.State.PID,
	}
	session.verifyRunning = func(verifyContext context.Context) (RunningInspection, error) {
		return inspectRunning(verifyContext, dependencies, plan, owner)
	}
	session.cleanupOwned.Store(true)
	published, foreign := eventGuard.publish(session)
	if !published {
		if foreign {
			cleanupOwned = false
		}
		recorder.Close()
		return fail()
	}
	// Once publication is possible, cancellation owns a cleanup callback.
	// Stopping it successfully is the linearization point at which the caller
	// receives the live handle; if it has already started, settle cleanup before
	// dropping the only handle to the private container ID.
	stopCancellationCleanup := context.AfterFunc(ctx, session.revokeBounded)
	if ctx.Err() != nil || !stopCancellationCleanup() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), sessionCleanupTimeout)
		cleanupErr := session.revokeUntilSettled(cleanupContext)
		cleanupCancel()
		if cleanupErr != nil {
			go session.revokeBounded()
		}
		return nil, ctx.Err()
	}
	return session, nil
}

func awaitArchiveBarrier(ctx context.Context, acknowledgement <-chan struct{}, events <-chan LifecycleEvent, eventErrors <-chan error, owner ownership) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, open := <-eventErrors:
			if !open {
				return ErrLifecycleDrift
			}
			return ErrLifecycleDrift
		case event, open := <-events:
			if !open {
				return ErrLifecycleDrift
			}
			if err := validateOwnership(owner, event.ContainerID, event.Labels); err != nil {
				return err
			}
			return ErrLifecycleDrift
		case <-acknowledgement:
			return nil
		}
	}
}

func validSessionDependencies(value sessionDependencies) bool {
	return value.engine != nil && value.prepareHost != nil && value.verifyHostArtifacts != nil && value.verifyContainerArtifacts != nil &&
		value.readProcess != nil && value.inspectSocket != nil && value.probeUnauthorized != nil &&
		value.newRecorder != nil && value.probeAuthenticated != nil && value.cleanupHost != nil
}

func sessionStartError(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrSessionUnavailable
}

func awaitStartEvent(ctx context.Context, events <-chan LifecycleEvent, eventErrors <-chan error, owner ownership) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, open := <-eventErrors:
			if !open {
				return ErrLifecycleDrift
			}
			return ErrLifecycleDrift
		case event, open := <-events:
			if !open {
				return ErrLifecycleDrift
			}
			state, err := advanceLifecycle(LifecycleCreated, owner, event)
			if err != nil {
				return err
			}
			if state != LifecycleRunning {
				return ErrLifecycleDrift
			}
			return nil
		}
	}
}

func inspectRunning(ctx context.Context, dependencies sessionDependencies, plan *referencePlan, owner ownership) (RunningInspection, error) {
	before, err := dependencies.engine.Inspect(ctx, owner.containerID)
	if err == nil {
		if ownershipErr := validateOwnership(owner, before.ID, before.Labels); ownershipErr != nil {
			return RunningInspection{}, ownershipErr
		}
	}
	if err != nil || !runningState(before.State) {
		return RunningInspection{}, ErrSessionUnavailable
	}
	process, err := dependencies.readProcess(ctx, before.State.PID)
	if err != nil {
		return RunningInspection{}, ErrSessionUnavailable
	}
	after, err := dependencies.engine.Inspect(ctx, owner.containerID)
	if err != nil {
		return RunningInspection{}, ErrSessionUnavailable
	}
	if ownershipErr := validateOwnership(owner, after.ID, after.Labels); ownershipErr != nil {
		return RunningInspection{}, ownershipErr
	}
	socket, err := dependencies.inspectSocket(ctx, dependencies.paths.RunDir)
	if err != nil {
		return RunningInspection{}, ErrSessionUnavailable
	}
	running := RunningInspection{Before: before, Process: process, After: after, Socket: socket}
	if err := validateRunning(plan, owner, running); err != nil {
		return RunningInspection{}, ErrSessionUnavailable
	}
	return running, nil
}

func newSessionEventGuard(ctx context.Context, events <-chan LifecycleEvent, eventErrors <-chan error, owner ownership) *sessionEventGuard {
	guard := &sessionEventGuard{commands: make(chan sessionEventCommand), done: make(chan struct{})}
	go guard.run(ctx, events, eventErrors, owner)
	return guard
}

func (guard *sessionEventGuard) run(ctx context.Context, events <-chan LifecycleEvent, eventErrors <-chan error, owner ownership) {
	defer close(guard.done)
	state := sessionEventState{}
	defer func() {
		guard.mu.Lock()
		guard.final = state
		guard.mu.Unlock()
	}()
	var session *ReferenceSession
	handleFailure := func(foreign bool) {
		state.fatal = true
		state.foreign = state.foreign || foreign
		state.published = false
		if session == nil {
			return
		}
		if foreign {
			session.cleanupOwned.Store(false)
		}
		session.revokeAuthority()
		go session.revokeBounded()
	}
	handleEvent := func(event LifecycleEvent, open bool) {
		if !open {
			handleFailure(false)
			return
		}
		next, err := advanceLifecycle(LifecycleRunning, owner, event)
		if err != nil || next != LifecycleRunning {
			handleFailure(errors.Is(err, ErrOwnershipLost))
		}
	}
	drainReady := func() {
		for {
			select {
			case _, open := <-eventErrors:
				_ = open
				handleFailure(false)
				return
			case event, open := <-events:
				handleEvent(event, open)
				if state.fatal {
					return
				}
			default:
				return
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-eventErrors:
			_ = open
			handleFailure(false)
			return
		case event, open := <-events:
			handleEvent(event, open)
			if state.fatal {
				return
			}
		case command := <-guard.commands:
			// The command is the serialization barrier between queued lifecycle
			// events and publication/cleanup. Drain everything already observable
			// before acknowledging it.
			drainReady()
			switch command.kind {
			case "publish":
				if !state.fatal && !state.foreign && command.session != nil {
					session = command.session
					state.published = true
				}
				command.response <- state
			case "stop":
				command.response <- state
				return
			default:
				handleFailure(false)
				command.response <- state
				return
			}
		}
	}
}

func (guard *sessionEventGuard) publish(session *ReferenceSession) (bool, bool) {
	if guard == nil || session == nil {
		return false, false
	}
	response := make(chan sessionEventState, 1)
	select {
	case guard.commands <- sessionEventCommand{kind: "publish", session: session, response: response}:
	case <-guard.done:
		state := guard.finalState()
		return false, state.foreign
	}
	state := <-response
	return state.published, state.foreign
}

func (guard *sessionEventGuard) stop() sessionEventState {
	if guard == nil {
		return sessionEventState{}
	}
	response := make(chan sessionEventState, 1)
	select {
	case guard.commands <- sessionEventCommand{kind: "stop", response: response}:
		return <-response
	case <-guard.done:
		return guard.finalState()
	}
}

func (guard *sessionEventGuard) finalState() sessionEventState {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return guard.final
}

func (session *ReferenceSession) revokeBounded() {
	ctx, cancel := context.WithTimeout(context.Background(), sessionCleanupTimeout)
	defer cancel()
	_ = session.revokeUntilSettled(ctx)
}

// Record performs one strict observation bracketed by fresh, full running-state
// validation. Docker events are only an eager negative signal: their absence is
// never used as evidence that this exact child remained authoritative.
func (session *ReferenceSession) Record(ctx context.Context, request rerank.ScoreRequest) (rerank.ReferenceRecording, error) {
	if session == nil || ctx == nil {
		return rerank.ReferenceRecording{}, ErrSessionUnavailable
	}
	if err := ctx.Err(); err != nil {
		return rerank.ReferenceRecording{}, err
	}
	session.mu.Lock()
	live, recorder, lifetime, anchorPID := session.live, session.recorder, session.lifetime, session.anchorPID
	session.mu.Unlock()
	if !live || recorder == nil || lifetime == nil || anchorPID <= 0 {
		return rerank.ReferenceRecording{}, ErrSessionUnavailable
	}
	if err := session.validateUseBoundary(ctx, lifetime, anchorPID); err != nil {
		return rerank.ReferenceRecording{}, err
	}
	callContext, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	defer func() { stop(); cancel() }()
	recording, err := recorder.Record(callContext, request)
	if callerErr := ctx.Err(); callerErr != nil {
		return rerank.ReferenceRecording{}, callerErr
	}
	if lifetime.Err() != nil {
		return rerank.ReferenceRecording{}, ErrSessionUnavailable
	}
	if err := session.validateUseBoundary(ctx, lifetime, anchorPID); err != nil {
		return rerank.ReferenceRecording{}, err
	}
	if err != nil {
		return rerank.ReferenceRecording{}, err
	}
	session.mu.Lock()
	stillLive := session.live && session.lifetime == lifetime && session.recorder == recorder && session.anchorPID == anchorPID
	session.mu.Unlock()
	if !stillLive {
		return rerank.ReferenceRecording{}, ErrSessionUnavailable
	}
	return recording, nil
}

// Evidence returns a detached, redacted point-in-time observation after a
// fresh full validation. It is output-only, not a lease, and cannot be decoded
// back into a session.
func (session *ReferenceSession) Evidence(ctx context.Context) (Evidence, error) {
	if session == nil || ctx == nil {
		return Evidence{}, ErrSessionUnavailable
	}
	session.mu.Lock()
	live, lifetime, anchorPID := session.live, session.lifetime, session.anchorPID
	session.mu.Unlock()
	if !live || lifetime == nil || anchorPID <= 0 {
		return Evidence{}, ErrSessionUnavailable
	}
	if err := session.validateUseBoundary(ctx, lifetime, anchorPID); err != nil {
		return Evidence{}, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.live || session.lifetime != lifetime || session.anchorPID != anchorPID {
		return Evidence{}, ErrSessionUnavailable
	}
	value := session.evidence
	value.redactedEnvironment = append([]string(nil), session.evidence.redactedEnvironment...)
	return value, nil
}

func (session *ReferenceSession) validateUseBoundary(ctx context.Context, lifetime context.Context, anchorPID int) error {
	if session == nil || ctx == nil || lifetime == nil || anchorPID <= 0 {
		return ErrSessionUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if lifetime.Err() != nil {
		return ErrSessionUnavailable
	}
	session.mu.Lock()
	verifier := session.verifyRunning
	live := session.live && session.lifetime == lifetime && session.anchorPID == anchorPID
	session.mu.Unlock()
	if !live || verifier == nil {
		return ErrSessionUnavailable
	}
	verifyContext, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	running, err := verifier(verifyContext)
	stop()
	cancel()
	if callerErr := ctx.Err(); callerErr != nil {
		return callerErr
	}
	if lifetime.Err() != nil {
		return ErrSessionUnavailable
	}
	if err != nil || running.Before.State.PID != anchorPID || running.After.State.PID != anchorPID {
		if errors.Is(err, ErrOwnershipLost) {
			session.cleanupOwned.Store(false)
		}
		session.revokeAuthority()
		go session.revokeBounded()
		return ErrSessionUnavailable
	}
	session.mu.Lock()
	stillLive := session.live && session.lifetime == lifetime && session.anchorPID == anchorPID
	session.mu.Unlock()
	if !stillLive {
		return ErrSessionUnavailable
	}
	return nil
}

// Done closes before cleanup starts whenever recording authority is revoked.
func (session *ReferenceSession) Done() <-chan struct{} {
	if session == nil || session.done == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return session.done
}

// Close atomically revokes the session, cancels in-flight recordings, waits
// for the recorder, then removes only the exact owned child.
func (session *ReferenceSession) Close(ctx context.Context) error {
	if session == nil || ctx == nil {
		return ErrSessionUnavailable
	}
	return session.revoke(ctx)
}

func (session *ReferenceSession) revoke(ctx context.Context) error {
	session.beginShutdown()
	cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), sessionCleanupTimeout)
	cleanupError := session.cleanupOnce(cleanupContext)
	cleanupCancel()
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return cleanupError
}

func (session *ReferenceSession) beginShutdown() {
	session.revokeAuthority()
	session.shutdownOnce.Do(func() {
		session.mu.Lock()
		recorder := session.recorder
		cancelEvents := session.cancelEvents
		session.mu.Unlock()
		if session.eventGuard != nil {
			state := session.eventGuard.stop()
			if state.foreign {
				session.cleanupOwned.Store(false)
			}
			<-session.eventGuard.done
		}
		if cancelEvents != nil {
			cancelEvents()
		}
		if recorder != nil {
			recorder.Close()
		}
	})
}

func (session *ReferenceSession) cleanupOnce(cleanupContext context.Context) error {
	session.cleanupMu.Lock()
	defer session.cleanupMu.Unlock()
	if !session.cleanupDone {
		var cleanupError error
		cleanupOwned := session.cleanupOwned.Load()
		if cleanupOwned && !session.containerCleaned {
			cleanupError = cleanupOwnedAttempt(cleanupContext, session.engine, session.plan, session.owner)
			if errors.Is(cleanupError, ErrOwnershipLost) {
				session.cleanupOwned.Store(false)
				cleanupOwned = false
			} else if cleanupError == nil {
				session.containerCleaned = true
			}
		}
		if cleanupOwned && session.containerCleaned && !session.hostCleaned && cleanupError == nil {
			if session.cleanupHost == nil {
				session.hostCleaned = true
			} else if err := session.cleanupHost(); err != nil {
				cleanupError = err
			} else {
				session.hostCleaned = true
			}
		}
		readyToCloseEngine := !cleanupOwned || session.containerCleaned && session.hostCleaned
		if readyToCloseEngine && !session.engineClosed && session.engine != nil {
			if err := session.engine.Close(); err != nil {
				cleanupError = err
			} else {
				session.engineClosed = true
			}
		}
		if cleanupError != nil {
			session.cleanupErr = ErrSessionUnavailable
		} else {
			session.cleanupErr = nil
		}
		session.cleanupDone = readyToCloseEngine && (session.engine == nil || session.engineClosed)
	}
	return session.cleanupErr
}

func (session *ReferenceSession) revokeUntilSettled(cleanupContext context.Context) error {
	if session == nil || cleanupContext == nil {
		return ErrSessionUnavailable
	}
	session.beginShutdown()
	for {
		cleanupError := session.cleanupOnce(cleanupContext)
		session.cleanupMu.Lock()
		settled := session.cleanupDone
		session.cleanupMu.Unlock()
		if settled {
			return cleanupError
		}
		if cleanupContext.Err() != nil {
			return ErrSessionUnavailable
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-cleanupContext.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ErrSessionUnavailable
		case <-timer.C:
		}
	}
}

func (session *ReferenceSession) revokeAuthority() {
	session.authorityOnce.Do(func() {
		session.mu.Lock()
		session.live = false
		cancel := session.cancel
		if cancel != nil {
			cancel()
		}
		if session.done != nil {
			close(session.done)
		}
		session.mu.Unlock()
	})
}

func cleanupOwnedUntilSettled(ctx context.Context, engine Engine, plan *referencePlan, owner ownership) error {
	if ctx == nil {
		return ErrSessionUnavailable
	}
	var last error
	for {
		last = cleanupOwnedAttempt(ctx, engine, plan, owner)
		if last == nil || errors.Is(last, ErrOwnershipLost) {
			return last
		}
		if ctx.Err() != nil {
			return ErrSessionUnavailable
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ErrSessionUnavailable
		case <-timer.C:
		}
	}
}

func cleanupOwnedAttempt(ctx context.Context, engine Engine, plan *referencePlan, owner ownership) error {
	if ctx == nil || engine == nil || plan == nil || owner.containerID == "" {
		return ErrSessionUnavailable
	}
	inspection, err := engine.InspectOwnership(ctx, owner.containerID)
	if err != nil {
		if errors.Is(err, errContainerNotFound) {
			return nil
		}
		return ErrSessionUnavailable
	}
	if validateOwnershipInspection(owner, inspection) != nil {
		return ErrOwnershipLost
	}
	if activeContainerState(inspection.State) {
		if err := engine.Kill(ctx, owner.containerID); err != nil {
			inspection, err = engine.InspectOwnership(ctx, owner.containerID)
			if err != nil {
				if errors.Is(err, errContainerNotFound) {
					return nil
				}
				return ErrSessionUnavailable
			}
			if validateOwnershipInspection(owner, inspection) != nil {
				return ErrOwnershipLost
			}
			if activeContainerState(inspection.State) {
				return ErrSessionUnavailable
			}
		} else {
			result, waitErr := engine.Wait(ctx, owner.containerID)
			inspection, err = engine.InspectOwnership(ctx, owner.containerID)
			if err != nil {
				if errors.Is(err, errContainerNotFound) {
					return nil
				}
				return ErrSessionUnavailable
			}
			if validateOwnershipInspection(owner, inspection) != nil {
				return ErrOwnershipLost
			}
			if activeContainerState(inspection.State) || waitErr == nil && result.ContainerID != owner.containerID {
				return ErrSessionUnavailable
			}
		}
	}
	if err := engine.Remove(ctx, owner.containerID); err != nil {
		if errors.Is(err, errContainerNotFound) {
			return nil
		}
		return ErrSessionUnavailable
	}
	return nil
}

func activeContainerState(state ContainerState) bool {
	return state.Running || state.Paused || state.Restarting || state.PID > 0
}
