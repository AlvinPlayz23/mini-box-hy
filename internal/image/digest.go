package image

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"
)

// Digest is a parsed content digest such as "sha256:abcd...".
type Digest struct {
	Algorithm string
	Hex       string
}

// ParseDigest parses an OCI content digest.
func ParseDigest(s string) (Digest, error) {
	var d Digest
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return d, fmt.Errorf("invalid digest %q", s)
	}
	d.Algorithm = strings.ToLower(s[:i])
	d.Hex = strings.ToLower(s[i+1:])
	if _, err := hex.DecodeString(d.Hex); err != nil {
		return d, fmt.Errorf("invalid digest %q: %w", s, err)
	}
	switch d.Algorithm {
	case "sha256":
		if len(d.Hex) != sha256.Size*2 {
			return d, fmt.Errorf("invalid sha256 digest length in %q", s)
		}
	case "sha512":
		if len(d.Hex) != sha512.Size*2 {
			return d, fmt.Errorf("invalid sha512 digest length in %q", s)
		}
	default:
		return d, fmt.Errorf("unsupported digest algorithm %q", d.Algorithm)
	}
	return d, nil
}

// String renders the digest.
func (d Digest) String() string { return d.Algorithm + ":" + d.Hex }

// NewHash returns a hash.Hash for the digest's algorithm.
func (d Digest) NewHash() (hash.Hash, error) {
	switch d.Algorithm {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("unsupported digest algorithm %q", d.Algorithm)
	}
}

// Verifier wraps a writer that computes a digest and reports a mismatch on Close.
type Verifier struct {
	d Digest
	h hash.Hash
}

// NewVerifier returns a Verifier for d.
func NewVerifier(d Digest) (*Verifier, error) {
	h, err := d.NewHash()
	if err != nil {
		return nil, err
	}
	return &Verifier{d: d, h: h}, nil
}

// Write implements io.Writer.
func (v *Verifier) Write(p []byte) (int, error) { return v.h.Write(p) }

// Close checks the accumulated content against the expected digest.
func (v *Verifier) Close() error {
	got := hex.EncodeToString(v.h.Sum(nil))
	if got != v.d.Hex {
		return fmt.Errorf("digest mismatch: expected %s, got %s:%s", v.d, v.d.Algorithm, got)
	}
	return nil
}

// VerifyDigest reads r fully and checks it against the expected digest string.
func VerifyDigest(r io.Reader, expected string) error {
	d, err := ParseDigest(expected)
	if err != nil {
		return err
	}
	v, err := NewVerifier(d)
	if err != nil {
		return err
	}
	if _, err := io.Copy(v, r); err != nil {
		return err
	}
	return v.Close()
}
