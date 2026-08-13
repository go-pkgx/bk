// Package bottlepkg packages a finished install tree into a pkgx bottle.
//
// A pkgx bottle is a gzip'd tar whose entries are all prefixed
// "<project>/v<version>/" (e.g. "openssl.org/v1.1.1w/bin/openssl"). The dist
// layout places each bottle at "<project>/<os>/<arch>/v<version>.tar.gz" next
// to a "versions.txt" listing the published versions, one per line.
package bottlepkg

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"io/fs"
	"path/filepath"
	"sort"

	"github.com/go-pkgx/bottle"
)

// Bottle walks installDir and writes a gzip'd tar to w in which every entry
// path is "<project>/v<version>/<relpath>". Regular files (with their mode
// bits), directories, and symlinks (tar TypeSymlink plus link target) are
// preserved. Hidden files are included; only the walk root itself is skipped.
// Paths are sorted for deterministic output.
func Bottle(installDir, project, version string, w io.Writer) error {
	var rels []string
	if err := walkDir(installDir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == installDir {
			return nil
		}
		rel, err := filepath.Rel(installDir, path)
		if err != nil {
			return err
		}
		rels = append(rels, rel)
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(rels)

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, rel := range rels {
		if err := addEntry(tw, installDir, project, version, rel); err != nil {
			tw.Close()
			gz.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
}

// addEntry appends the entry at installDir/rel to tw under the bottle prefix.
func addEntry(tw *tar.Writer, installDir, project, version, rel string) error {
	full := filepath.Join(installDir, rel)
	fi, err := osLstat(full)
	if err != nil {
		return err
	}
	link := ""
	if fi.Mode()&fs.ModeSymlink != 0 {
		if link, err = osReadlink(full); err != nil {
			return err
		}
	}
	hdr, err := tarFileInfoHeader(fi, link)
	if err != nil {
		return err
	}
	hdr.Name = project + "/v" + version + "/" + filepath.ToSlash(rel)
	if fi.IsDir() {
		hdr.Name += "/"
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	f, err := osOpen(full)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = ioCopy(tw, f)
	return err
}

// WriteBottle creates "outDir/<project>/<os>/<arch>/v<version>.tar.gz",
// bottles installDir into it, and appends version to the sibling
// "versions.txt". It returns the tarball path.
func WriteBottle(installDir, project, version, os, arch, outDir string) (string, error) {
	dir := filepath.Join(outDir, project, os, arch)
	if err := osMkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tarballPath := filepath.Join(dir, "v"+version+bottle.ExtTarGz)
	f, err := osCreate(tarballPath)
	if err != nil {
		return "", err
	}
	if err := Bottle(installDir, project, version, f); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	vf, err := osOpenFileAppend(filepath.Join(dir, "versions.txt"))
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(vf, version+"\n"); err != nil {
		vf.Close()
		return "", err
	}
	if err := vf.Close(); err != nil {
		return "", err
	}
	return tarballPath, nil
}
