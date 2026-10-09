package image

import (
	"strings"
	"testing"
)

func TestParseReference(t *testing.T) {
	tests := []struct {
		in       string
		registry string
		repo     string
		tag      string
		digest   string
		wantErr  bool
	}{
		{in: "alpine", registry: "docker.io", repo: "library/alpine", tag: "latest"},
		{in: "alpine:3.20", registry: "docker.io", repo: "library/alpine", tag: "3.20"},
		{in: "docker.io/library/alpine:3.20", registry: "docker.io", repo: "library/alpine", tag: "3.20"},
		{in: "ghcr.io/owner/app:v1", registry: "ghcr.io", repo: "owner/app", tag: "v1"},
		{in: "localhost:5000/team/app:dev", registry: "localhost:5000", repo: "team/app", tag: "dev"},
		{in: "registry.example.com:8443/a/b/c", registry: "registry.example.com:8443", repo: "a/b/c", tag: "latest"},
		{in: "Alpine:3.20", registry: "docker.io", repo: "library/alpine", tag: "3.20"},
		{in: "UPPER/case:1", registry: "docker.io", repo: "upper/case", tag: "1"},
		{in: "alpine@sha256:" + hex64, registry: "docker.io", repo: "library/alpine", digest: "sha256:" + hex64},
		{in: "docker.io/library/alpine@sha256:" + hex64, registry: "docker.io", repo: "library/alpine", digest: "sha256:" + hex64},
		{in: "", wantErr: true},
		{in: "alpine:", wantErr: true},
		{in: "alpine@sha256:short", wantErr: true},
		{in: "ghcr.io/owner/app@notadigest", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseReference(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseReference(%q) = %+v, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseReference(%q) error: %v", tt.in, err)
			continue
		}
		if got.Registry != tt.registry || got.Repository != tt.repo || got.Tag != tt.tag || got.Digest != tt.digest {
			t.Errorf("ParseReference(%q) = %+v, want registry=%q repo=%q tag=%q digest=%q",
				tt.in, got, tt.registry, tt.repo, tt.tag, tt.digest)
		}
	}
}

// hex64 is a syntactically valid 64-character hex digest.
var hex64 = strings.Repeat("a", 64)

func TestReferenceNameAndFamiliar(t *testing.T) {
	r, err := ParseReference("alpine:3.20")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Name(), "docker.io/library/alpine:3.20"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if got, want := r.Familiar(), "alpine:3.20"; got != want {
		t.Errorf("Familiar() = %q, want %q", got, want)
	}

	r, err = ParseReference("ghcr.io/owner/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Familiar(), "ghcr.io/owner/app:v1"; got != want {
		t.Errorf("Familiar() = %q, want %q", got, want)
	}
}

func TestReferenceRoundTrip(t *testing.T) {
	for _, in := range []string{
		"docker.io/library/alpine:3.20",
		"ghcr.io/owner/app:v1",
		"localhost:5000/team/app:dev",
		"docker.io/library/alpine@sha256:" + hex64,
	} {
		r, err := ParseReference(in)
		if err != nil {
			t.Fatalf("ParseReference(%q): %v", in, err)
		}
		if r.Name() != in {
			t.Errorf("round trip %q -> %q", in, r.Name())
		}
		again, err := ParseReference(r.Name())
		if err != nil {
			t.Fatalf("re-parse %q: %v", r.Name(), err)
		}
		if again != r {
			t.Errorf("re-parse mismatch: %+v != %+v", again, r)
		}
	}
}

func FuzzParseReference(f *testing.F) {
	for _, s := range []string{"alpine", "alpine:3.20", "ghcr.io/a/b:c", "alpine@sha256:" + hex64, "..", "a/../b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := ParseReference(s)
		if err != nil {
			return
		}
		// A successfully parsed reference must round-trip through its own Name().
		again, err := ParseReference(r.Name())
		if err != nil {
			t.Fatalf("re-parse %q (from %q): %v", r.Name(), s, err)
		}
		if again != r {
			t.Fatalf("round trip mismatch for %q: %+v != %+v", s, again, r)
		}
	})
}
