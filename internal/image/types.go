package image

import "time"

// Media types for OCI and Docker manifests.
const (
	MediaTypeOCIIndex           = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerConfig       = "application/vnd.docker.container.image.v1+json"
	MediaTypeOCIConfig          = "application/vnd.oci.image.config.v1+json"

	MediaTypeLayerGzip         = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	MediaTypeLayerZstd         = "application/vnd.oci.image.layer.v1.tar+zstd"
	MediaTypeLayerUncompressed = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeLayerOCIGzip      = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	URLs        []string          `json:"urls,omitempty"`
	Platform    *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Manifest is an OCI image manifest.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Index is an OCI image index (manifest list).
type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Manifests     []Descriptor `json:"manifests"`
}

// ConfigFile is the image configuration blob (the OCI image config).
type ConfigFile struct {
	Architecture string         `json:"architecture"`
	OS           string         `json:"os"`
	Variant      string         `json:"variant,omitempty"`
	Created      time.Time      `json:"created,omitempty"`
	Config       ImageConfig    `json:"config"`
	RootFS       RootFS         `json:"rootfs"`
	History      []HistoryEntry `json:"history,omitempty"`
}

// RootFS lists the uncompressed layer digests of an image.
type RootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

// HistoryEntry is one build step record.
type HistoryEntry struct {
	Created    time.Time `json:"created,omitempty"`
	CreatedBy  string    `json:"created_by,omitempty"`
	EmptyLayer bool      `json:"empty_layer,omitempty"`
}

// ImageConfig is the runtime-relevant part of an image config.
type ImageConfig struct {
	User         string              `json:"User,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
}

// LayerRef records one layer of a stored image.
type LayerRef struct {
	Digest    string `json:"digest"`
	DiffID    string `json:"diff_id,omitempty"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type,omitempty"`
}

// Record is a stored image: the manifest, its layers and the resolved config.
type Record struct {
	Ref          string     `json:"ref"`
	Registry     string     `json:"registry"`
	Repository   string     `json:"repository"`
	Tag          string     `json:"tag,omitempty"`
	Digest       string     `json:"digest"`
	ConfigDigest string     `json:"config_digest"`
	Layers       []LayerRef `json:"layers"`
	ChainID      string     `json:"chain_id"`
	Size         int64      `json:"size"`
	Created      time.Time  `json:"created"`
	Config       ConfigFile `json:"config"`
}

// RootfsDir returns the flattened directory for this image's layer chain.
func (r *Record) RootfsDir(layersDir string) string {
	return layersDir + "/" + r.ChainID + "/diff"
}

// Platform returns the image platform.
func (r *Record) Platform() Platform {
	return Platform{OS: r.Config.OS, Architecture: r.Config.Architecture, Variant: r.Config.Variant}
}
