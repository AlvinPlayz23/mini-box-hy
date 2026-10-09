package image

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestParseDigest(t *testing.T) {
	good := "sha256:" + hex64
	d, err := ParseDigest(good)
	if err != nil {
		t.Fatalf("ParseDigest(%q): %v", good, err)
	}
	if d.Algorithm != "sha256" || d.String() != good {
		t.Errorf("got %+v", d)
	}
	for _, bad := range []string{"", "sha256", "sha256:", "md5:" + hex64, "sha256:zz", "sha256:abcd"} {
		if _, err := ParseDigest(bad); err == nil {
			t.Errorf("ParseDigest(%q) succeeded, want error", bad)
		}
	}
}

func TestVerifyDigest(t *testing.T) {
	content := "hello"
	sum := sha256.Sum256([]byte(content))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if err := VerifyDigest(strings.NewReader(content), want); err != nil {
		t.Fatalf("VerifyDigest: %v", err)
	}
	// Flip a byte: "hellp".
	if err := VerifyDigest(strings.NewReader("hellp"), want); err == nil {
		t.Fatal("VerifyDigest accepted corrupted content")
	}
}
