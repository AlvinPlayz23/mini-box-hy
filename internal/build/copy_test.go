package build

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// setupBuildContext builds a context with files, a glob target, a symlink to a
// file inside the context and symlinks pointing outside of it.
func setupBuildContext(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	ctx := filepath.Join(base, "context")
	writeFile(t, filepath.Join(ctx, "app", "main.go"), "package main\n")
	writeFile(t, filepath.Join(ctx, "build", "a.go"), "package build\n")
	writeFile(t, filepath.Join(ctx, "build", "b.go"), "package build\n")
	writeFile(t, filepath.Join(ctx, "build", "sub", "c.go"), "package sub\n")
	writeFile(t, filepath.Join(ctx, "README.md"), "hi\n")
	writeFile(t, filepath.Join(base, "outside.txt"), "secret\n")
	if err := os.Symlink(filepath.Join(base, "outside.txt"), filepath.Join(ctx, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ctx, "app", "main.go"), filepath.Join(ctx, "inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ctx, "links"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside.txt"), filepath.Join(ctx, "links", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ctx, "app", "main.go"), filepath.Join(ctx, "links", "inside")); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestExpandCopySourcesExact(t *testing.T) {
	ctx := setupBuildContext(t)
	tests := []struct {
		pattern string
		want    []string
	}{
		{pattern: "app/main.go", want: []string{"app/main.go"}},
		{pattern: "./app/main.go", want: []string{"app/main.go"}},
		{pattern: "build/", want: []string{"build"}},
		{pattern: "inside", want: []string{"inside"}},
	}
	for _, tt := range tests {
		got, err := ExpandCopySources(ctx, tt.pattern)
		if err != nil {
			t.Errorf("ExpandCopySources(ctx, %q) error: %v", tt.pattern, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ExpandCopySources(ctx, %q) = %v, want %v", tt.pattern, got, tt.want)
		}
	}

	if _, err := ExpandCopySources(ctx, "nope.txt"); err == nil {
		t.Error("ExpandCopySources with a missing file succeeded, want error")
	}
}

func TestExpandCopySourcesGlob(t *testing.T) {
	ctx := setupBuildContext(t)
	got, err := ExpandCopySources(ctx, "build/*")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build/a.go", "build/b.go", "build/sub"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandCopySources(ctx, \"build/*\") = %v, want %v", got, want)
	}

	got, err = ExpandCopySources(ctx, "build/*.go")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"build/a.go", "build/b.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandCopySources(ctx, \"build/*.go\") = %v, want %v", got, want)
	}

	got, err = ExpandCopySources(ctx, "links/inside")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"links/inside"}) {
		t.Errorf("symlink inside the context = %v, want [links/inside]", got)
	}
}

func TestExpandCopySourcesReject(t *testing.T) {
	ctx := setupBuildContext(t)
	for _, bad := range []string{
		"../outside.txt",
		"/etc/passwd",
		"app/../../outside.txt",
		"escape",
		"links/*",
		"missing/*",
	} {
		if _, err := ExpandCopySources(ctx, bad); err == nil {
			t.Errorf("ExpandCopySources(ctx, %q) succeeded, want error", bad)
		}
	}
}
