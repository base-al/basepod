// Package tarpack packs a directory into a deterministic, gzipped tar
// stream — BasePod's build-context format, shared by every producer of
// one: the CLI's `basepod deploy` (a local directory), the git cloner
// (internal/gitsource, a shallow checkout), and compose's per-service
// context repack. It moved here (out of internal/cli, where it started
// as v0.1/v0.3's private `basepod deploy` packer) specifically so
// server-side code can consume it too — see the v0.5 git+compose plan's
// Task 1.
//
// This is a pure move: PackDir, PackToTempFile, and HasContainerfile
// behave identically to their unexported internal/cli predecessors
// (packDir, packToTempFile, hasContainerfile) — same .basepodignore
// handling, same unconditional .git exclusion, same deterministic
// entry ordering/mtime, same symlink-skip behavior. internal/cli now
// re-exports thin wrappers around these (see internal/cli/tar.go) so
// `basepod deploy`'s output stays byte-identical.
package tarpack

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// ignoreFileName is the .basepodignore file's name at a build context's
// root — a gitignore-style subset (see ignoreRule) of patterns to exclude
// from the deploy tarball. It is always excluded from the tarball itself
// (there's no reason to ship it into the build context), alongside .git/,
// which is excluded unconditionally regardless of what .basepodignore
// says.
const ignoreFileName = ".basepodignore"

// deterministicModTime is the fixed mtime stamped onto every tar entry
// (regardless of the source file's real mtime), so PackDir's output
// depends only on the build context's file contents and layout — not on
// when it happened to be packed. Combined with fs.WalkDir's own guaranteed
// lexical directory-entry order, this makes PackDir's output byte-for-byte
// reproducible across runs for the same input.
var deterministicModTime = time.Unix(0, 0).UTC()

// ignoreRule is one parsed, non-comment, non-blank line from
// .basepodignore: a gitignore-style subset supporting a leading "/" (anchor
// the match to the build context root rather than any depth) and a
// trailing "/" (match a directory — and, implicitly via WalkDir pruning,
// everything under it — rather than requiring an exact file). The pattern
// itself may contain "*", matched via filepath.Match.
type ignoreRule struct {
	pattern  string
	anchored bool
	dirOnly  bool
}

// parseIgnoreRules parses .basepodignore syntax from r: blank lines and
// lines starting with "#" are skipped; every other line becomes one
// ignoreRule.
func parseIgnoreRules(r io.Reader) ([]ignoreRule, error) {
	var rules []ignoreRule
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule := ignoreRule{pattern: line}
		if strings.HasPrefix(rule.pattern, "/") {
			rule.anchored = true
			rule.pattern = rule.pattern[1:]
		}
		if strings.HasSuffix(rule.pattern, "/") {
			rule.dirOnly = true
			rule.pattern = strings.TrimSuffix(rule.pattern, "/")
		}
		if rule.pattern == "" {
			continue
		}
		rules = append(rules, rule)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return rules, nil
}

// loadIgnoreRules reads dir/.basepodignore if present, returning an empty
// (nil) rule set — not an error — if it doesn't exist.
func loadIgnoreRules(dir string) ([]ignoreRule, error) {
	f, err := os.Open(filepath.Join(dir, ignoreFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return parseIgnoreRules(f)
}

// matchIgnore reports whether relPath (slash-separated, relative to the
// build context root) is excluded by rules. relPath is checked as a whole
// against anchored rules, and by its base name (the final path segment)
// against unanchored ones — matching gitignore's own "a pattern with no
// slash matches at any depth" convention for this subset.
func matchIgnore(rules []ignoreRule, relPath string, isDir bool) bool {
	base := path.Base(relPath)
	for _, r := range rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.anchored {
			if r.dirOnly {
				if relPath == r.pattern || strings.HasPrefix(relPath, r.pattern+"/") {
					return true
				}
				continue
			}
			if ok, _ := filepath.Match(r.pattern, relPath); ok {
				return true
			}
			continue
		}
		if ok, _ := filepath.Match(r.pattern, base); ok {
			return true
		}
	}
	return false
}

// HasContainerfile reports whether dir contains a root-level Containerfile
// or Dockerfile (a regular file, matching internal/build's own check) —
// the same requirement the server enforces on an uploaded build context,
// checked client-side so `basepod deploy` can fail fast before packing and
// uploading anything.
func HasContainerfile(dir string) bool {
	for _, name := range []string{"Containerfile", "Dockerfile"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// PackDir tars and gzips dir's contents into w, honoring dir/.basepodignore
// (see loadIgnoreRules/matchIgnore) — always excluding .git/ and
// .basepodignore itself regardless of what that file says. Entries are
// written in fs.WalkDir's lexical order with a fixed mtime/uid/gid (see
// deterministicModTime), so the same input directory always produces
// byte-identical output. Only regular files and directories are included;
// symlinks and other special files are silently skipped.
func PackDir(dir string, w io.Writer) error {
	rules, err := loadIgnoreRules(dir)
	if err != nil {
		return fmt.Errorf("tarpack: read %s: %w", ignoreFileName, err)
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		if relSlash == ".git" || strings.HasPrefix(relSlash, ".git/") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if relSlash == ignoreFileName {
			return nil
		}
		if matchIgnore(rules, relSlash, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return nil // symlinks aren't portable into a container build context
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if d.IsDir() {
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = relSlash + "/"
			hdr.ModTime = deterministicModTime
			hdr.Uid, hdr.Gid = 0, 0
			hdr.Uname, hdr.Gname = "", ""
			return tw.WriteHeader(hdr)
		}

		if !info.Mode().IsRegular() {
			return nil // sockets, devices, etc. — not valid build-context input
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = relSlash
		hdr.ModTime = deterministicModTime
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if walkErr != nil {
		return walkErr
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// PackToTempFile packs dir (see PackDir) into a fresh temp file and
// returns it open for reading from the start, along with its size. The
// caller is responsible for closing the file and removing it (its Name())
// once done — deploy uses this so the tarball's exact byte size is known
// up front (for Content-Length) without buffering the whole thing in
// memory.
func PackToTempFile(dir string) (f *os.File, size int64, err error) {
	tmp, err := os.CreateTemp("", "basepod-deploy-*.tar.gz")
	if err != nil {
		return nil, 0, err
	}
	if err := PackDir(dir, tmp); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, err
	}
	info, err := tmp.Stat()
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, err
	}
	return tmp, info.Size(), nil
}
