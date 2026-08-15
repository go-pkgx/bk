// Package fetch downloads and extracts pkgx package sources.
//
// It supports the archive formats used by pantry distributables
// (.tar.gz/.tgz, .tar.xz, .tar.bz2/.tbz2, .tar, .zip) plus shallow git
// clones via the system git binary. Extraction is pure Go, preserves file
// modes, recreates symlinks, and rejects archive entries whose paths are
// absolute or would escape the destination directory.
package fetch

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-pkgx/bottle"
	"github.com/ulikunitz/xz"
)

// Archive kinds detected from a URL's file extension.
const (
	kindTarGz  = "tar.gz"
	kindTarXz  = "tar.xz"
	kindTarBz2 = "tar.bz2"
	kindTar    = "tar"
	kindZip    = "zip"
)

// Fetch downloads url and extracts the archive into destDir, stripping
// stripComponents leading path components from every entry (like
// `tar --strip-components=N`). The archive format is detected from the
// URL's extension: .tar.gz/.tgz, .tar.xz, .tar.bz2/.tbz2, .tar, .zip.
func Fetch(url, destDir string, stripComponents int) error {
	kind := detect(url)
	if kind == "" {
		return fmt.Errorf("fetch: unknown archive extension in %q", url)
	}
	resp, err := httpGet(url)
	if err != nil {
		return fmt.Errorf("fetch: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch: GET %s: %s", url, resp.Status)
	}
	if err := osMkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("fetch: create %s: %w", destDir, err)
	}
	switch kind {
	case kindTarGz:
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return fmt.Errorf("fetch: read gzip from %s: %w", url, err)
		}
		defer gz.Close()
		return extractTar(tar.NewReader(gz), destDir, stripComponents)
	case kindTarXz:
		xr, err := xz.NewReader(resp.Body)
		if err != nil {
			return fmt.Errorf("fetch: read xz from %s: %w", url, err)
		}
		return extractTar(tar.NewReader(xr), destDir, stripComponents)
	case kindTarBz2:
		return extractTar(tar.NewReader(bzip2.NewReader(resp.Body)), destDir, stripComponents)
	case kindTar:
		return extractTar(tar.NewReader(resp.Body), destDir, stripComponents)
	default: // kindZip
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("fetch: read body of %s: %w", url, err)
		}
		return extractZip(data, destDir, stripComponents)
	}
}

// ExtractTarGzFile extracts the local gzip-compressed tar at src into destDir,
// stripping strip leading path components (like `tar --strip-components=N`). It
// reuses the same hardened extractor as Fetch, so absolute or dest-escaping
// entries are rejected rather than written. bkpyvenv's poetry seal uses it to
// unpack a freshly built sdist into the in-project venv's site-packages.
func ExtractTarGzFile(src, destDir string, strip int) error {
	f, err := osOpen(src)
	if err != nil {
		return fmt.Errorf("fetch: open %s: %w", src, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("fetch: read gzip from %s: %w", src, err)
	}
	defer gz.Close()
	if err := osMkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("fetch: create %s: %w", destDir, err)
	}
	return extractTar(tar.NewReader(gz), destDir, strip)
}

// FetchGit shallow-clones ref of repoURL into destDir with go-git's pure-Go
// clone — NO `git` binary dependency. `git clone --branch <ref>` accepts either
// a tag or a branch, so try the ref as a tag first, then as a branch (cleaning
// the destination between attempts, since PlainClone needs an empty target).
func FetchGit(repoURL, ref, destDir string) error {
	// pkgx marks a git distributable with a "git+" URL prefix (e.g.
	// git+https://github.com/o/r); strip it to the plain transport scheme that
	// go-git (and git) actually speak.
	repoURL = strings.TrimPrefix(repoURL, "git+")
	var lastErr error
	for _, rn := range []plumbing.ReferenceName{
		plumbing.NewTagReferenceName(ref),
		plumbing.NewBranchReferenceName(ref),
	} {
		_ = osRemoveAll(destDir)
		_, err := gitPlainClone(destDir, false, &gogit.CloneOptions{
			URL: repoURL, ReferenceName: rn, Depth: 1, SingleBranch: true, Tags: gogit.NoTags,
		})
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("fetch: git clone %s@%s: %w", repoURL, ref, lastErr)
}

// detect maps a URL to an archive kind by extension, ignoring any query
// string or fragment. It returns "" for unknown extensions.
func detect(url string) string {
	name := strings.ToLower(url)
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	switch {
	case strings.HasSuffix(name, bottle.ExtTarGz), strings.HasSuffix(name, ".tgz"):
		return kindTarGz
	case strings.HasSuffix(name, bottle.ExtTarXz):
		return kindTarXz
	case strings.HasSuffix(name, ".tar.bz2"), strings.HasSuffix(name, ".tbz2"):
		return kindTarBz2
	case strings.HasSuffix(name, ".zip"):
		return kindZip
	case strings.HasSuffix(name, ".tar"):
		return kindTar
	}
	return ""
}

// safeTarget resolves an archive entry name to a path under destDir after
// stripping strip leading components. ok is false when the entry should be
// skipped (fully consumed by stripping). Absolute entry names and names
// escaping destDir yield an error.
func safeTarget(destDir, name string, strip int) (target string, ok bool, err error) {
	if path.IsAbs(name) || filepath.IsAbs(filepath.FromSlash(name)) {
		return "", false, fmt.Errorf("fetch: absolute path %q in archive", name)
	}
	parts := strings.Split(path.Clean(name), "/")
	if len(parts) <= strip {
		return "", false, nil
	}
	rel := path.Join(parts[strip:]...)
	if rel == "." {
		return "", false, nil
	}
	target = filepath.Join(destDir, filepath.FromSlash(rel))
	prefix := filepath.Clean(destDir) + string(filepath.Separator)
	if !strings.HasPrefix(target+string(filepath.Separator), prefix) {
		return "", false, fmt.Errorf("fetch: path traversal %q in archive", name)
	}
	return target, true, nil
}

// permOr returns the permission bits of m, or fallback when m carries none
// (e.g. zip entries written without Unix modes).
func permOr(m, fallback fs.FileMode) fs.FileMode {
	if p := m.Perm(); p != 0 {
		return p
	}
	return fallback
}

// writeFile creates target with perm and copies r into it.
func writeFile(target string, perm fs.FileMode, r io.Reader) error {
	f, err := osOpenFile(target, osWriteFlags, perm)
	if err != nil {
		return fmt.Errorf("fetch: create %s: %w", target, err)
	}
	if _, err := ioCopy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("fetch: write %s: %w", target, err)
	}
	return f.Close()
}

// extractTar extracts every entry of tr into destDir, stripping strip leading
// path components. It delegates to the shared bottle.Extract, the pkgx
// ecosystem's single tar extractor: it strips components, restores each regular
// file's recorded mtime (tar(1) semantics — recipes such as libexpat 2.8.3
// depend on generated files staying newer than their sources), rejects absolute
// or directory-escaping names (and hard-link sources) with bottle.ErrInsecurePath,
// and reproduces dirs, regular files, symlinks and hard links; unsupported entry
// types (fifos, devices, ...) are skipped.
func extractTar(tr *tar.Reader, destDir string, strip int) error {
	return bottle.Extract(tr, destDir, strip)
}

// restoreTime gives an extracted file the modification time the archive
// recorded, exactly as tar(1) does. Recipes DEPEND on this: a release tarball
// ships generated files NEWER than the sources they derive from precisely so
// make leaves them alone. Stamping every file with "now" instead re-orders them
// by archive position, and make then tries to regenerate — libexpat 2.8.3 dies
// that way (doc/xmlwf.1 is archived before, hence stamped older than,
// doc/xmlwf.xml → make runs the docbook rule with no docbook2x-man installed →
// `test "x" != x` → Error 1), and the same trap is behind the autotools
// maintainer-mode rebuilds MAKEFLAGS/TouchAutotools work around.
//
// An archive with no recorded time (zero) is left as written.
func restoreTime(path string, mt time.Time) error {
	if mt.IsZero() {
		return nil
	}
	if err := osChtimes(path, mt, mt); err != nil {
		return fmt.Errorf("fetch: set mtime %s: %w", path, err)
	}
	return nil
}

// extractZip extracts a zip archive held in data into destDir, stripping
// strip leading path components. As with tar, zip.ErrInsecurePath is
// tolerated because safeTarget performs its own (stricter) vetting.
func extractZip(data []byte, destDir string, strip int) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return fmt.Errorf("fetch: read zip: %w", err)
	}
	for _, f := range zr.File {
		target, ok, err := safeTarget(destDir, f.Name, strip)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := osMkdirAll(target, permOr(mode, 0o755)); err != nil {
				return fmt.Errorf("fetch: create %s: %w", target, err)
			}
		case mode&fs.ModeSymlink != 0:
			rc, err := zipOpen(f)
			if err != nil {
				return fmt.Errorf("fetch: open %s in zip: %w", f.Name, err)
			}
			var link strings.Builder
			_, err = ioCopy(&link, rc)
			rc.Close()
			if err != nil {
				return fmt.Errorf("fetch: read %s in zip: %w", f.Name, err)
			}
			if err := osMkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("fetch: create %s: %w", filepath.Dir(target), err)
			}
			if err := osSymlink(link.String(), target); err != nil {
				return fmt.Errorf("fetch: symlink %s: %w", target, err)
			}
		default:
			if err := osMkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("fetch: create %s: %w", filepath.Dir(target), err)
			}
			rc, err := zipOpen(f)
			if err != nil {
				return fmt.Errorf("fetch: open %s in zip: %w", f.Name, err)
			}
			err = writeFile(target, permOr(mode, 0o644), rc)
			rc.Close()
			if err != nil {
				return err
			}
			if err := restoreTime(target, f.Modified); err != nil {
				return err
			}
		}
	}
	return nil
}
