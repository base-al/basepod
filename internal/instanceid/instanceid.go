// Package instanceid gives each running BasePod daemon a stable identity
// distinct from every other BasePod daemon that might be pointed at the
// same Podman socket (issue #10): a dev instance next to a production one,
// two test instances, or a throwaway instance started against a scratch
// data dir all share one libpod API, and "is this container mine?" cannot
// be answered by the basepod.managed=true label alone — it's identical
// across every install. This package's LoadOrCreate mints a random id once
// per data dir and persists it, so every resource BasePod creates (app
// containers, bp-caddy, the basepod network) can be stamped
// basepod.instance=<id>, and every "clean up what's mine" path (orphan GC,
// Caddy drift/recreate — see internal/deploy and internal/caddy) can
// filter on it instead.
package instanceid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// idBytes is how many random bytes back the id (32 bytes = 256 bits,
// hex-encoded to a 64-character string) — deliberately generous: unlike
// crypto.LoadOrCreateKey's secret.key, this value is never secret (it's
// stamped in plaintext onto every container's labels, readable by anyone
// who can already reach the Podman socket), so the only property that
// matters is that two independently-booted instances essentially never
// collide.
const idBytes = 32

// fileName is the file LoadOrCreate reads/creates, a sibling of
// crypto.LoadOrCreateKey's secret.key in the same data dir.
const fileName = "instance.id"

// loadOrCreateMu guards concurrent LoadOrCreate calls within the same
// process, mirroring crypto.LoadOrCreateKey's loadOrCreateKeyMu exactly —
// see that function's doc comment for why: without it, two goroutines
// racing a first boot could each generate a different id before either
// finishes writing, and whichever write happened to lose the rename race
// would silently leave the loser's in-memory copy inconsistent with what's
// actually on disk. Multi-process first-run races remain best-effort for
// the same reason LoadOrCreateKey's are: a single daemon owns dataDir.
var loadOrCreateMu sync.Mutex

// LoadOrCreate loads or creates this instance's stable id from
// <dataDir>/instance.id. It creates the parent directory structure if
// needed, writes the file with 0600 permissions, and returns the same id
// on every subsequent call for the same dataDir — for the lifetime of the
// data dir, this id never changes (it is not regenerated on restart, and
// nothing in BasePod ever rewrites it once created).
//
// The write path mirrors crypto.LoadOrCreateKey exactly (see its doc
// comment for the full rationale): write to a temp file, rename it into
// place atomically so a crash or disk-full error never leaves a truncated
// id file, then re-read the persisted id from disk rather than trusting
// the in-memory value just generated — so that if a second process won a
// concurrent first-boot race and wrote its own id first, this call
// (whichever process it's running in) returns the one actually on disk,
// making the operation idempotent across processes, not just goroutines.
func LoadOrCreate(dataDir string) (string, error) {
	loadOrCreateMu.Lock()
	defer loadOrCreateMu.Unlock()

	idPath := filepath.Join(dataDir, fileName)

	if data, err := os.ReadFile(idPath); err == nil {
		id := strings.TrimSpace(string(data))
		if id == "" {
			return "", fmt.Errorf("instanceid: existing id file %s is empty", idPath)
		}
		return id, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("instanceid: reading id file: %w", err)
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("instanceid: creating data dir: %w", err)
	}

	raw := make([]byte, idBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("instanceid: generating id: %w", err)
	}
	id := hex.EncodeToString(raw)

	tempPath := idPath + ".tmp"
	if err := os.WriteFile(tempPath, []byte(id), 0o600); err != nil {
		return "", fmt.Errorf("instanceid: writing temporary id file: %w", err)
	}
	if err := os.Rename(tempPath, idPath); err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("instanceid: finalizing id file: %w", err)
	}

	persisted, err := os.ReadFile(idPath)
	if err != nil {
		return "", fmt.Errorf("instanceid: reading persisted id file: %w", err)
	}
	persistedID := strings.TrimSpace(string(persisted))
	if persistedID == "" {
		return "", fmt.Errorf("instanceid: persisted id file %s is empty", idPath)
	}
	return persistedID, nil
}
