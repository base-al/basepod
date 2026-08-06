package instanceid

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestLoadOrCreateCreatesFile proves a first call creates instance.id with
// 0600 permissions and a non-empty id.
func TestLoadOrCreateCreatesFile(t *testing.T) {
	dataDir := t.TempDir()

	id, err := LoadOrCreate(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if id == "" {
		t.Fatal("LoadOrCreate returned an empty id")
	}

	idPath := filepath.Join(dataDir, "instance.id")
	info, err := os.Stat(idPath)
	if err != nil {
		t.Fatalf("instance.id not found: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("instance.id mode = %o, want 0600", mode)
	}
}

// TestLoadOrCreateStableAcrossRestarts proves the id returned by a second,
// independent call against the same data dir (simulating a process
// restart) is byte-for-byte identical to the first — the id must never be
// regenerated once created, or every container/network label stamped with
// the old id would suddenly look "foreign" to the next boot.
func TestLoadOrCreateStableAcrossRestarts(t *testing.T) {
	dataDir := t.TempDir()

	id1, err := LoadOrCreate(dataDir)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}

	id2, err := LoadOrCreate(dataDir)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}

	if id1 != id2 {
		t.Errorf("id changed across calls: first=%q second=%q", id1, id2)
	}

	// A third call, reading the file back exactly as a fresh process
	// restart would, must also agree.
	id3, err := LoadOrCreate(dataDir)
	if err != nil {
		t.Fatalf("third LoadOrCreate: %v", err)
	}
	if id3 != id1 {
		t.Errorf("id changed on third call: %q vs %q", id3, id1)
	}
}

// TestLoadOrCreateDifferentDataDirsDifferentIDs proves two distinct data
// dirs (i.e. two distinct BasePod instances) never mint the same id —
// this is the whole property issue #10's fix depends on.
func TestLoadOrCreateDifferentDataDirsDifferentIDs(t *testing.T) {
	idA, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate A: %v", err)
	}
	idB, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate B: %v", err)
	}
	if idA == idB {
		t.Fatalf("two distinct data dirs minted the same instance id: %q", idA)
	}
}

// TestLoadOrCreateConcurrentFirstBoot proves a concurrent first-boot race
// (many goroutines calling LoadOrCreate against the same never-before-seen
// data dir at once — e.g. two request paths in the same process both
// resolving the instance id before either has run yet) is serialized by
// loadOrCreateMu: every goroutine must observe the exact same id, and the
// file on disk must end up well-formed (not truncated or double-written).
func TestLoadOrCreateConcurrentFirstBoot(t *testing.T) {
	dataDir := t.TempDir()

	const n = 32
	ids := make([]string, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = LoadOrCreate(dataDir)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: LoadOrCreate: %v", i, err)
		}
	}
	want := ids[0]
	if want == "" {
		t.Fatal("first goroutine returned an empty id")
	}
	for i, id := range ids {
		if id != want {
			t.Errorf("goroutine %d returned id %q, want %q (all goroutines must agree)", i, id, want)
		}
	}

	// The file on disk must match what every goroutine observed — proving
	// the race didn't leave a half-written or inconsistent file behind.
	onDisk, err := LoadOrCreate(dataDir)
	if err != nil {
		t.Fatalf("post-race LoadOrCreate: %v", err)
	}
	if onDisk != want {
		t.Errorf("on-disk id %q != what goroutines agreed on %q", onDisk, want)
	}
}

// TestLoadOrCreateRejectsEmptyFile proves a zero-byte (or whitespace-only)
// existing instance.id — e.g. from a crash between create and write on some
// hypothetical future code path, or manual tampering — is reported as an
// error rather than silently treated as a valid (empty) id, which would
// make every "is this labeled for my instance" comparison in orphan GC and
// the Caddy manager trivially true for basepod.instance="" containers.
func TestLoadOrCreateRejectsEmptyFile(t *testing.T) {
	dataDir := t.TempDir()
	idPath := filepath.Join(dataDir, "instance.id")
	if err := os.WriteFile(idPath, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadOrCreate(dataDir); err == nil {
		t.Error("LoadOrCreate should reject a whitespace-only existing id file")
	}
}

// TestLoadOrCreateCreatesMissingParent mirrors
// crypto.TestLoadOrCreateKeyCreatesMissingParent: LoadOrCreate must create
// the full data dir path, not just write into an already-existing one.
func TestLoadOrCreateCreatesMissingParent(t *testing.T) {
	nested := filepath.Join(t.TempDir(), "nested", "dir", "structure")

	id, err := LoadOrCreate(nested)
	if err != nil {
		t.Fatalf("LoadOrCreate failed to create parent directories: %v", err)
	}
	if id == "" {
		t.Fatal("LoadOrCreate returned an empty id")
	}

	info, err := os.Stat(filepath.Join(nested, "instance.id"))
	if err != nil {
		t.Fatalf("instance.id not found in nested directory: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("instance.id mode = %o, want 0600", mode)
	}
}
