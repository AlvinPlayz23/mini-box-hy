package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCacheKeyDeterministic(t *testing.T) {
	in := Instruction{Cmd: "RUN", Args: "go build ./...", Line: 3}
	k1 := CacheKey("base", in, "ctx")
	k2 := CacheKey("base", in, "ctx")
	if k1 != k2 {
		t.Errorf("CacheKey not stable: %s vs %s", k1, k2)
	}
	if len(k1) != 64 {
		t.Errorf("CacheKey = %q, want a 64-char hex digest", k1)
	}
}

func TestCacheKeySensitivity(t *testing.T) {
	base := Instruction{Cmd: "COPY", Args: "app /app", Flags: map[string]string{"from": "0"}, Line: 4}
	want := CacheKey("stage0", base, "ctxhash")

	tests := []struct {
		name string
		key  string
		in   Instruction
		hash string
	}{
		{"same input", "stage0", base, "ctxhash"},
		{"changed base", "stage1", base, "ctxhash"},
		{"changed args", "stage0", Instruction{Cmd: "COPY", Args: "app2 /app", Flags: base.Flags, Line: 4}, "ctxhash"},
		{"changed cmd", "stage0", Instruction{Cmd: "RUN", Args: "app /app", Flags: base.Flags, Line: 4}, "ctxhash"},
		{"changed flags", "stage0", Instruction{Cmd: "COPY", Args: "app /app", Flags: map[string]string{"from": "1"}, Line: 4}, "ctxhash"},
		{"changed context", "stage0", base, "ctxhash2"},
		{"exec instead of shell", "stage0", Instruction{Cmd: "COPY", Exec: []string{"app", "/app"}, Flags: base.Flags, Line: 4}, "ctxhash"},
	}
	for _, tt := range tests {
		got := CacheKey(tt.key, tt.in, tt.hash)
		if tt.name == "same input" {
			if got != want {
				t.Errorf("%s: key = %s, want %s", tt.name, got, want)
			}
			continue
		}
		if got == want {
			t.Errorf("%s: cache key unchanged", tt.name)
		}
	}

	lineShifted := base
	lineShifted.Line = 99
	if CacheKey("stage0", lineShifted, "ctxhash") != want {
		t.Error("cache key changed when only the line number changed")
	}
}

func TestContextHashContent(t *testing.T) {
	ctx := t.TempDir()
	writeFile(t, filepath.Join(ctx, "app", "main.go"), "package main\n")
	writeFile(t, filepath.Join(ctx, "app", "util.go"), "package main\n")
	writeFile(t, filepath.Join(ctx, "README.md"), "hi\n")

	h1, err := ContextHash(ctx, []string{"app/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h1, "sha256:") {
		t.Errorf("hash %q missing sha256: prefix", h1)
	}

	// Unrelated edits must not change the hash.
	writeFile(t, filepath.Join(ctx, "app", "util.go"), "package main // changed\n")
	writeFile(t, filepath.Join(ctx, "README.md"), "bye\n")
	h2, err := ContextHash(ctx, []string{"app/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("hash changed after unrelated edits: %s vs %s", h1, h2)
	}

	// Changing a copied file must change the hash.
	writeFile(t, filepath.Join(ctx, "app", "main.go"), "package main // changed\n")
	h3, err := ContextHash(ctx, []string{"app/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h3 {
		t.Error("hash unchanged after the copied file changed")
	}
}

func TestContextHashScope(t *testing.T) {
	ctx := t.TempDir()
	writeFile(t, filepath.Join(ctx, "app", "main.go"), "package main\n")
	writeFile(t, filepath.Join(ctx, "app", "util.go"), "package main\n")
	writeFile(t, filepath.Join(ctx, "README.md"), "hi\n")

	dirHash, err := ContextHash(ctx, []string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	globHash, err := ContextHash(ctx, []string{"app/*"})
	if err != nil {
		t.Fatal(err)
	}
	if dirHash != globHash {
		t.Errorf("directory pattern %s != glob pattern %s, want equal", dirHash, globHash)
	}

	writeFile(t, filepath.Join(ctx, "app", "util.go"), "package main // changed\n")
	dirHash2, err := ContextHash(ctx, []string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if dirHash == dirHash2 {
		t.Error("directory hash unchanged after a file inside it changed")
	}

	// The same content in a different directory hashes identically.
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "app", "main.go"), "package main\n")
	writeFile(t, filepath.Join(other, "app", "util.go"), "package main // changed\n")
	writeFile(t, filepath.Join(other, "README.md"), "hi\n")
	otherHash, err := ContextHash(other, []string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if otherHash != dirHash2 {
		t.Errorf("same content in another directory: %s != %s", otherHash, dirHash2)
	}
}

func TestContextHashPatternMiss(t *testing.T) {
	ctx := t.TempDir()
	writeFile(t, filepath.Join(ctx, "app", "main.go"), "package main\n")

	_, err := ContextHash(ctx, []string{"app/main.go", "ghost.txt"})
	if err == nil {
		t.Fatal("ContextHash succeeded with a pattern matching nothing")
	}
	if !strings.Contains(err.Error(), "ghost.txt") {
		t.Errorf("error %q does not name the unmatched pattern", err)
	}

	if _, err := ContextHash(ctx, nil); err != nil {
		t.Errorf("ContextHash with no patterns: %v", err)
	}
}
