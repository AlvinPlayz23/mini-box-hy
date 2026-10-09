// Package image implements OCI image references, digests, the content-addressed
// blob/layer store, registry pulling and layer unpacking.
package image

import (
	"fmt"
	"regexp"
	"strings"
)

// Defaults used when a reference omits the registry, namespace or tag.
const (
	DefaultRegistry  = "docker.io"
	DefaultNamespace = "library"
	DefaultTag       = "latest"
)

// Reference is a parsed container image reference. Exactly one of Tag or Digest
// is set after a successful parse.
type Reference struct {
	Registry   string // e.g. docker.io, ghcr.io, localhost:5000
	Repository string // e.g. library/alpine
	Tag        string // e.g. 3.20
	Digest     string // e.g. sha256:abcd... (takes precedence over Tag)
}

var (
	digestRe = regexp.MustCompile(`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-f0-9]{32,}$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	repoRe   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*)*$`)
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*(:[0-9]+)?$`)
)

// ParseReference parses a Docker-style image reference. It accepts shorthand
// forms such as "alpine", "alpine:3.20", "docker.io/library/alpine:3.20",
// "ghcr.io/owner/app@sha256:..." and "localhost:5000/team/app:dev".
func ParseReference(s string) (Reference, error) {
	var ref Reference
	s = strings.TrimSpace(s)
	if s == "" {
		return ref, fmt.Errorf("empty image reference")
	}

	rest := s
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		ref.Digest = rest[i+1:]
		rest = rest[:i]
		if !digestRe.MatchString(ref.Digest) {
			return ref, fmt.Errorf("invalid digest %q in reference %q", ref.Digest, s)
		}
	}

	ref.Registry = DefaultRegistry
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		first := rest[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			if !hostRe.MatchString(first) {
				return ref, fmt.Errorf("invalid registry host %q in reference %q", first, s)
			}
			ref.Registry = first
			rest = rest[i+1:]
		}
	}

	// A tag is the last ':' in the final path component.
	if i := strings.LastIndexByte(rest, ':'); i >= 0 && !strings.ContainsRune(rest[i+1:], '/') {
		ref.Tag = rest[i+1:]
		rest = rest[:i]
		if ref.Tag == "" {
			return ref, fmt.Errorf("empty tag in reference %q", s)
		}
	}

	if rest == "" {
		return ref, fmt.Errorf("missing repository in reference %q", s)
	}
	repo := strings.ToLower(rest)
	if !strings.ContainsRune(repo, '/') && ref.Registry == DefaultRegistry {
		repo = DefaultNamespace + "/" + repo
	}
	if !repoRe.MatchString(repo) {
		return ref, fmt.Errorf("invalid repository %q in reference %q", repo, s)
	}
	ref.Repository = repo

	if ref.Digest != "" {
		return ref, nil
	}
	if ref.Tag == "" {
		ref.Tag = DefaultTag
	}
	if !tagRe.MatchString(ref.Tag) {
		return ref, fmt.Errorf("invalid tag %q in reference %q", ref.Tag, s)
	}
	return ref, nil
}

// Name returns the fully-qualified reference, e.g. "docker.io/library/alpine:3.20".
func (r Reference) Name() string {
	base := r.Registry + "/" + r.Repository
	if r.Digest != "" {
		return base + "@" + r.Digest
	}
	return base + ":" + r.Tag
}

// String implements fmt.Stringer.
func (r Reference) String() string { return r.Name() }

// Familiar renders the reference the way a user typed it, dropping the
// docker.io/library prefix where it is implied.
func (r Reference) Familiar() string {
	repo := r.Repository
	if r.Registry == DefaultRegistry {
		repo = strings.TrimPrefix(repo, DefaultNamespace+"/")
	} else {
		repo = r.Registry + "/" + repo
	}
	if r.Digest != "" {
		return repo + "@" + r.Digest
	}
	return repo + ":" + r.Tag
}

// IsDigest reports whether the reference is pinned by digest.
func (r Reference) IsDigest() bool { return r.Digest != "" }

// RepositoryPath is the on-disk path (relative to the image store) for this
// reference, keyed by registry/repository. The tag or digest is a filename.
func (r Reference) RepositoryPath() string {
	return r.Registry + "/" + r.Repository
}

// Identifier is the tag or digest that names the image within a repository.
func (r Reference) Identifier() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}
