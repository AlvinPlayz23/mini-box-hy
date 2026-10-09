// Package api serves a Docker-Engine-API-compatible HTTP subset over the
// minibox runtime, so Docker-style clients, IDEs and Portainer can talk to it.
package api

import (
	"context"
	"encoding/json"
	"io"
)

// Backend is what the API needs from the runtime. The cli package implements it.
type Backend interface {
	// Ping checks that the runtime is healthy.
	Ping(ctx context.Context) error
	// Version reports runtime and host version details.
	Version(ctx context.Context) (VersionInfo, error)
	// ListContainers reports every container when all is set, otherwise
	// just the running ones.
	ListContainers(ctx context.Context, all bool) ([]ContainerSummary, error)
	// InspectContainer returns the container's raw JSON description.
	InspectContainer(ctx context.Context, id string) (json.RawMessage, error)
	// CreateContainer creates a container from req and returns its id.
	CreateContainer(ctx context.Context, req CreateRequest) (CreateResponse, error)
	// StartContainer starts a created or stopped container.
	StartContainer(ctx context.Context, id string) error
	// StopContainer stops a container, waiting at most timeoutSeconds.
	StopContainer(ctx context.Context, id string, timeoutSeconds int) error
	// KillContainer signals a container.
	KillContainer(ctx context.Context, id, signal string) error
	// RemoveContainer removes a container, killing it first when force is set.
	RemoveContainer(ctx context.Context, id string, force bool) error
	// WaitContainer blocks until the container exits and reports its exit code.
	WaitContainer(ctx context.Context, id string) (int, error)
	// ContainerLogs returns the container's plain-text log; tail is the
	// number of lines from the end, negative for all. The caller closes the
	// reader.
	ContainerLogs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error)
	// ListImages reports the images in local storage.
	ListImages(ctx context.Context) ([]ImageSummary, error)
	// InspectImage returns the image's raw JSON description.
	InspectImage(ctx context.Context, name string) (json.RawMessage, error)
	// PullImage pulls ref, calling progress with human-readable status
	// lines before it returns.
	PullImage(ctx context.Context, ref string, progress func(string)) error
}

// VersionInfo is the /version response.
type VersionInfo struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	GitCommit     string `json:"GitCommit,omitempty"`
	GoVersion     string `json:"GoVersion"`
	Os            string `json:"Os"`
	Arch          string `json:"Arch"`
	KernelVersion string `json:"KernelVersion,omitempty"`
}

// ContainerSummary is the /containers/json element, shaped like Docker's.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Ports   []PortSummary     `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
	Health  string            `json:"Health,omitempty"` // starting, healthy, unhealthy
}

// PortSummary is one published port of a container.
type PortSummary struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

// CreateRequest accepts the Docker fields minibox supports; encoding/json
// silently ignores unknown fields.
type CreateRequest struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Entrypoint []string          `json:"Entrypoint"`
	Env        []string          `json:"Env"`
	WorkingDir string            `json:"WorkingDir"`
	User       string            `json:"User"`
	Hostname   string            `json:"Hostname"`
	Tty        bool              `json:"Tty"`
	OpenStdin  bool              `json:"OpenStdin"`
	Labels     map[string]string `json:"Labels"`
	HostConfig HostConfig        `json:"HostConfig"`
}

// HostConfig carries the host-level container options minibox supports.
type HostConfig struct {
	Binds          []string                 `json:"Binds"`
	Memory         int64                    `json:"Memory"`
	NanoCPUs       int64                    `json:"NanoCpus"`
	PidsLimit      *int64                   `json:"PidsLimit"`
	AutoRemove     bool                     `json:"AutoRemove"`
	ReadonlyRootfs bool                     `json:"ReadonlyRootfs"`
	NetworkMode    string                   `json:"NetworkMode"`
	CapAdd         []string                 `json:"CapAdd"`
	CapDrop        []string                 `json:"CapDrop"`
	Tmpfs          map[string]string        `json:"Tmpfs"`
	RestartPolicy  RestartPolicy            `json:"RestartPolicy"`
	PortBindings   map[string][]PortBinding `json:"PortBindings"`
}

// PortBinding maps a container port to a host port.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// RestartPolicy is the container's restart policy.
type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

// CreateResponse is the /containers/create reply.
type CreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings,omitempty"`
}

// ImageSummary is the /images/json element.
type ImageSummary struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	Created     int64    `json:"Created"`
	Size        int64    `json:"Size"`
	VirtualSize int64    `json:"VirtualSize"`
}
