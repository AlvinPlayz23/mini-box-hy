//go:build linux

package container

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	t.Setenv(RootEnv, root)
	s, err := OpenAt(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreLayout(t *testing.T) {
	s := testStore(t)
	for _, dir := range []string{s.ContainersDir(), s.ImagesDir(), s.LayersDir(), s.BlobsDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("missing %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s has mode %o, want 700", dir, perm)
		}
	}
}

func TestDefaultRootUsesEnv(t *testing.T) {
	t.Setenv(RootEnv, "/tmp/minibox-test-root")
	if got := DefaultRoot(); got != "/tmp/minibox-test-root" {
		t.Errorf("DefaultRoot = %q", got)
	}
}

func TestNewIDIsUniqueAndLong(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 64 {
			t.Fatalf("id %q has length %d, want 64", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestResolvePrefixAndName(t *testing.T) {
	s := testStore(t)
	id1 := strings.Repeat("a", 64)
	id2 := strings.Repeat("b", 64)
	for _, id := range []string{id1, id2} {
		if err := os.MkdirAll(s.Dir(id), 0o700); err != nil {
			t.Fatal(err)
		}
		st := &State{ID: id, Status: StatusExited, CreatedAt: time.Now()}
		if id == id1 {
			st.Name = "web"
		}
		if err := st.Save(filepath.Join(s.Dir(id), "state.json")); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Resolve("aaaaaaaa")
	if err != nil || got != id1 {
		t.Errorf("Resolve(prefix) = %q, %v", got, err)
	}
	got, err = s.Resolve("web")
	if err != nil || got != id1 {
		t.Errorf("Resolve(name) = %q, %v", got, err)
	}
	if _, err := s.Resolve("missing"); err == nil {
		t.Error("Resolve(missing) succeeded")
	}
	if _, err := s.Resolve(""); err == nil {
		t.Error("Resolve(\"\") succeeded")
	}
}

func TestResolveAmbiguousPrefix(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{strings.Repeat("a", 64), "ab" + strings.Repeat("c", 62)} {
		if err := os.MkdirAll(s.Dir(id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Resolve("a"); err == nil {
		t.Fatal("Resolve(\"a\") should be ambiguous")
	} else if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error %v does not mention ambiguity", err)
	}
}

func TestLockIsExclusive(t *testing.T) {
	s := testStore(t)
	id := strings.Repeat("f", 64)
	l1, err := s.LockContainer(id)
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Release()

	done := make(chan struct{})
	var mu sync.Mutex
	acquired := false
	go func() {
		defer close(done)
		l2, err := s.LockContainer(id)
		if err != nil {
			return
		}
		mu.Lock()
		acquired = true
		mu.Unlock()
		l2.Release()
	}()

	select {
	case <-done:
		mu.Lock()
		got := acquired
		mu.Unlock()
		if got {
			t.Fatal("second lock acquired while first was held")
		}
	case <-time.After(200 * time.Millisecond):
		// Expected: the goroutine is still blocked.
	}
	l1.Release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("lock was not released")
	}
	mu.Lock()
	defer mu.Unlock()
	if !acquired {
		t.Error("second lock never acquired after release")
	}
}

func TestStateRoundTripAndRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st := &State{
		ID: "abc", Name: "test", Status: StatusRunning,
		PID: os.Getpid(), StartedAt: time.Now(), CreatedAt: time.Now(),
	}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != st.ID || got.Status != st.Status || got.PID != st.PID || got.Name != "test" {
		t.Errorf("round trip mismatch: %+v", got)
	}
	got.Refresh()
	if got.Status != StatusRunning {
		t.Errorf("live pid refreshed to %q", got.Status)
	}

	got.PID = 1 << 30
	got.Refresh()
	if got.Status != StatusExited {
		t.Errorf("dead pid refreshed to %q, want exited", got.Status)
	}
}

func TestLoadStateMissingFile(t *testing.T) {
	st, err := LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing state file should not error: %v", err)
	}
	if st.ID != "" {
		t.Errorf("expected zero state, got %+v", st)
	}
}

func TestPruneRemovesDeadContainers(t *testing.T) {
	s := testStore(t)
	dead := strings.Repeat("d", 64)
	live := strings.Repeat("e", 64)
	for _, id := range []string{dead, live} {
		if err := os.MkdirAll(s.Dir(id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := (&State{ID: dead, Status: StatusExited, CreatedAt: time.Now()}).Save(filepath.Join(s.Dir(dead), "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := (&State{ID: live, Status: StatusRunning, PID: os.Getpid(), CreatedAt: time.Now()}).Save(filepath.Join(s.Dir(live), "state.json")); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Prune()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != dead {
		t.Errorf("prune removed %v, want [%s]", removed, dead)
	}
	if s.Exists(dead) {
		t.Error("dead container still present")
	}
	if !s.Exists(live) {
		t.Error("running container was pruned")
	}
}
