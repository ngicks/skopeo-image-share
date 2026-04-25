package skopeoimageshare

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Media types we care about. Other types are not rejected outright —
// the parser only requires that .config.digest and .layers[].digest be
// present.
const (
	MediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// Descriptor is the OCI/Docker descriptor shared shape: an addressable
// blob reference with media type, digest and size.
type Descriptor struct {
	MediaType string `json:"mediaType,omitempty"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size,omitempty"`
}

// Manifest is the minimal subset of an OCI v1 / Docker v2.2 image
// manifest needed to derive the blob set: schema version, media type,
// the config descriptor, and the layer descriptor list.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// LayerDigests returns the layer digests in order.
func (m Manifest) LayerDigests() []string {
	out := make([]string, 0, len(m.Layers))
	for _, l := range m.Layers {
		out = append(out, l.Digest)
	}
	return out
}

// Index is the minimal subset of an OCI image index / Docker manifest
// list: an outer descriptor list. We use it only when reading a dumped
// `<tag>/index.json` that holds exactly one manifest descriptor.
type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Manifests     []Descriptor `json:"manifests"`
}

// ParseManifest decodes a single image manifest. It returns an error
// for top-level index/list documents — those should be parsed as
// [Index].
func ParseManifest(data []byte) (Manifest, error) {
	if len(data) == 0 {
		return Manifest{}, errors.New("manifest: empty input")
	}
	var probe struct {
		MediaType string       `json:"mediaType"`
		Manifests []Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(data, &probe); err == nil {
		switch probe.MediaType {
		case MediaTypeOCIIndex, MediaTypeDockerList:
			return Manifest{}, fmt.Errorf("manifest: got index/list mediaType %q, expected single manifest", probe.MediaType)
		}
		if len(probe.Manifests) > 0 && probe.MediaType == "" {
			return Manifest{}, errors.New("manifest: input looks like an index (has .manifests[]) but no mediaType")
		}
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if m.Config.Digest == "" {
		return Manifest{}, errors.New("manifest: missing config.digest")
	}
	return m, nil
}

// ParseIndex decodes an OCI image index / Docker manifest list.
func ParseIndex(data []byte) (Index, error) {
	if len(data) == 0 {
		return Index{}, errors.New("index: empty input")
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return Index{}, fmt.Errorf("index: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return Index{}, errors.New("index: empty .manifests[]")
	}
	return idx, nil
}
