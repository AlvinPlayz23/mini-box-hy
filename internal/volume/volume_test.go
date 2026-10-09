//go:build linux

package volume

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "volumes"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"data", "my-vol", "a_b", "App.Data", "v1"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "a/b", "a b", "..", "a:b"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) succeeded, want error", bad)
		}
	}
}

func TestIsName(t *testing.T) {
	for _, name := range []string{"data", "my-vol", "app_data"} {
		if !IsName(name) {
			t.Errorf("IsName(%q) = false, want true", name)
		}
	}
	for _, path := range []string{"/var/data", "./data", "../data", "~/data", ""} {
		if IsName(path) {
			t.Errorf("IsName(%q) = true, want false", path)
		}
	}
}

func TestCreateGetList(t *testing.T) {
	s := testStore(t)
	v, err := s.Create("data")
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "data" || v.Mountpoint != s.DataDir("data") {
		t.Errorf("volume = %+v", v)
	}
	info, err := os.Stat(s.DataDir("data"))
	if err != nil || !info.IsDir() {
		t.Fatalf("data dir missing: %v", err)
	}
	// Creating again is idempotent and keeps the original creation time.
	again, err := s.Create("data")
	if err != nil {
		t.Fatal(err)
	}
	if !again.Created.Equal(v.Created) {
		t.Error("Create replaced an existing volume")
	}
	if _, err := s.Create("other"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "data" || list[1].Name != "other" {
		t.Errorf("list = %+v", list)
	}
}

func TestGetMissing(t *testing.T) {
	s := testStore(t)
	_, err := s.Get("nope")
	if err == nil {
		t.Fatal("Get(missing) succeeded")
	}
	if !strings.Contains(err.Error(), "volume create") {
		t.Errorf("error does not say what to do: %v", err)
	}
}

func TestEnsureCreates(t *testing.T) {
	s := testStore(t)
	if _, err := s.Ensure("fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("fresh"); err != nil {
		t.Fatalf("Ensure did not create the volume: %v", err)
	}
}

func TestRemoveRefusesInUse(t *testing.T) {
	s := testStore(t)
	if _, err := s.Create("busy"); err != nil {
		t.Fatal(err)
	}
	inUse := map[string]bool{"busy": true}
	if err := s.Remove("busy", false, inUse); err == nil {
		t.Fatal("Remove of an in-use volume succeeded")
	} else if !strings.Contains(err.Error(), "-f") {
		t.Errorf("error does not mention force: %v", err)
	}
	if err := s.Remove("busy", true, inUse); err != nil {
		t.Fatalf("forced removal failed: %v", err)
	}
	if _, err := os.Stat(s.dir("busy")); !os.IsNotExist(err) {
		t.Error("volume directory survived removal")
	}
}

func TestPruneSkipsInUse(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"keep", "drop1", "drop2"} {
		if _, err := s.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.Prune(map[string]bool{"keep": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("prune removed %v, want two volumes", removed)
	}
	if _, err := s.Get("keep"); err != nil {
		t.Error("prune removed an in-use volume")
	}
}

func TestNamesInUse(t *testing.T) {
	inUse := NamesInUse([][]string{
		{"data:/var/lib/data", "/host/path:/mnt", "cache:/cache:ro"},
		{"./rel:/rel"},
	})
	if !inUse["data"] || !inUse["cache"] {
		t.Errorf("inUse = %v, want data and cache", inUse)
	}
	if inUse["/host/path"] || inUse["./rel"] {
		t.Errorf("host paths were treated as volume names: %v", inUse)
	}
}
