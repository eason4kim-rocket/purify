// Package dockerengine defines the bounded Docker Engine state used by the
// recording-only R-6a supervisor. It deliberately contains no Docker client,
// production scorer, serialized admission token, or certified registry entry.
package dockerengine

import (
	"context"
	"errors"
	"io"

	"github.com/use-agent/purify/search/rerank/deploy"
)

const (
	DockerSocketPath             = "/var/run/docker.sock"
	MinimumEngineAPIVersion      = "1.49"
	UserNamespaceDisabled        = "disabled"
	ReferenceImageRepository     = "vllm/vllm-openai"
	ReferenceImageManifestDigest = "sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0"
	ReferenceImageConfigID       = deploy.ReferenceImageConfigID
	ReferenceManifestMediaType   = "application/vnd.docker.distribution.manifest.v2+json"
	ReferenceComponent           = "r6a-vllm"

	ReferenceSnapshotPath = deploy.ReferenceSnapshotPath
	ReferenceTemplatePath = "/run/purify/qwen3_reranker.jinja"
	ReferenceRuntimePath  = "/run/purify"
	ReferenceSocketPath   = deploy.ReferenceUDSPath

	NetworkModeNone = "none"
	IPCModePrivate  = "private"
	RestartPolicyNo = "no"
	MountTypeBind   = "bind"

	LabelManaged    = "purify.managed"
	LabelComponent  = "purify.component"
	LabelRunID      = "purify.run_id"
	LabelSpecDigest = "purify.spec_digest"

	ContainerStatusCreated = "created"
	ContainerStatusRunning = "running"
	ContainerStatusExited  = "exited"

	LifecycleActionCreate  = "create"
	LifecycleActionStart   = "start"
	LifecycleActionKill    = "kill"
	LifecycleActionDie     = "die"
	LifecycleActionDestroy = "destroy"
)

var (
	ErrAdmissionRejected = errors.New("rerank docker engine: admission rejected")
	ErrImageRejected     = errors.New("rerank docker engine: image rejected")
	ErrContainerRejected = errors.New("rerank docker engine: container rejected")
	ErrOwnershipLost     = errors.New("rerank docker engine: ownership lost")
	ErrLifecycleDrift    = errors.New("rerank docker engine: lifecycle drift")
)

// Engine is intentionally narrower than a general Docker client. In
// particular, it cannot pull, list, rename, adopt, exec in, or copy arbitrary
// files into containers. A concrete adapter is added separately from this
// pure state package.
type Engine interface {
	InspectDaemon(context.Context) (DaemonInspection, error)
	InspectImage(context.Context, string) (ImageInspection, error)
	Create(context.Context, CreateSpec) (CreateResult, error)
	Start(context.Context, string) error
	Inspect(context.Context, string) (ContainerInspection, error)
	// ReadReferenceArchive is restricted by the concrete adapter to the exact
	// pinned snapshot or template path. It is not a generic container-copy API.
	ReadReferenceArchive(context.Context, string, string) (io.ReadCloser, error)
	Events(context.Context, string) (<-chan LifecycleEvent, <-chan error)
	Kill(context.Context, string) error
	Wait(context.Context, string) (WaitResult, error)
	Remove(context.Context, string) error
}

type ControllerInspection struct {
	Endpoint          string
	GOOS              string
	GOARCH            string
	EffectiveUIDKnown bool
	EffectiveUID      int
}

type DaemonInspection struct {
	APIVersion        string
	OSType            string
	Architecture      string
	Rootful           bool
	UserNamespaceMode string
}

type ManifestDescriptor struct {
	Digest    string
	MediaType string
}

type ImageConfigInspection struct {
	Entrypoint   []string
	Cmd          []string
	CmdPresent   bool
	Environment  []string
	User         string
	WorkingDir   string
	ExposedPorts []string
	Volumes      []string
}

type ImageInspection struct {
	RequestedReference string
	ID                 string
	OS                 string
	Architecture       string
	ManifestDescriptor *ManifestDescriptor
	Config             ImageConfigInspection
}

type HostPaths struct {
	SnapshotDir  string
	TemplateFile string
	RunDir       string
}

type Mount struct {
	Type        string
	Source      string
	Destination string
	ReadOnly    bool
}

type TmpfsMount struct {
	Destination string
	SizeBytes   int64
	Mode        uint32
}

type DeviceRequest struct {
	Driver       string
	Count        int64
	DeviceIDs    []string
	Capabilities [][]string
	Options      map[string]string
}

type PortBinding struct {
	ContainerPort string
	HostIP        string
	HostPort      string
}

// CreateSpec is the semantic input handed to the bounded Engine adapter. The
// environment is deliberately excluded from generic JSON serialization: it
// contains the in-memory API key required by the child.
type CreateSpec struct {
	ImageID        string
	Hostname       string
	Entrypoint     []string
	Command        []string
	Environment    []string `json:"-"`
	User           string
	WorkingDir     string
	Labels         map[string]string
	Mounts         []Mount
	Tmpfs          []TmpfsMount
	DeviceRequests []DeviceRequest
	NetworkMode    string
	IPCMode        string
	ShmSizeBytes   int64
	ReadOnlyRootFS bool
	RestartPolicy  string
	Privileged     bool
	TTY            bool
	ExposedPorts   []string
	PortBindings   []PortBinding
	CapAdd         []string
	Links          []string
}

type CreateResult struct {
	ContainerID string
	Warnings    []string
}

type WaitResult struct {
	ContainerID string
	ExitCode    int64
}

type ContainerState struct {
	Status     string
	Running    bool
	Paused     bool
	Restarting bool
	Dead       bool
	OOMKilled  bool
	PID        int
	ExitCode   int
}

// ContainerInspection is a normalized Docker inspect record. An adapter must
// populate both image identity and the manifest descriptor; absence is not
// treated as an older-daemon compatibility success.
type ContainerInspection struct {
	ID                      string
	ImageID                 string
	ConfiguredImage         string
	ImageManifestDescriptor *ManifestDescriptor
	Platform                string
	Path                    string
	Args                    []string
	Hostname                string
	Entrypoint              []string
	Command                 []string
	Environment             []string
	User                    string
	WorkingDir              string
	Labels                  map[string]string
	Mounts                  []Mount
	Tmpfs                   []TmpfsMount
	DeviceRequests          []DeviceRequest
	NetworkMode             string
	IPCMode                 string
	ShmSizeBytes            int64
	ReadOnlyRootFS          bool
	RestartPolicy           string
	Privileged              bool
	TTY                     bool
	ExposedPorts            []string
	PortBindings            []PortBinding
	CapAdd                  []string
	Links                   []string
	RestartCount            int
	State                   ContainerState
}

type ProcessInspection struct {
	PID         int
	Environment []string
}

type SocketInspection struct {
	Path      string
	IsSocket  bool
	IsSymlink bool
	Fresh     bool
}

// RunningInspection binds a bounded /proc read to the same running PID before
// and after the read. This prevents a stale PID or self-reported child env from
// becoming deployment evidence.
type RunningInspection struct {
	Before  ContainerInspection
	Process ProcessInspection
	After   ContainerInspection
	Socket  SocketInspection
}

type LifecycleEvent struct {
	ContainerID string
	Action      string
	Labels      map[string]string
}

type LifecycleState string

const (
	LifecycleNew      LifecycleState = "new"
	LifecycleCreated  LifecycleState = "created"
	LifecycleRunning  LifecycleState = "running"
	LifecycleStopping LifecycleState = "stopping"
	LifecycleExited   LifecycleState = "exited"
	LifecycleRemoved  LifecycleState = "removed"
)

// Evidence has no exported fields and no decoding/admission constructor.
// MarshalJSON is its only serialization surface.
type Evidence struct {
	containerID         string
	runID               string
	specDigest          string
	imageDigest         string
	imageConfigID       string
	platform            string
	environmentDigest   string
	redactedEnvironment []string
	state               string
}
