// Package bottlepkg packages a finished install tree into a pkgx bottle.
//
// A pkgx bottle is a compressed tar whose entries are all prefixed
// "<project>/v<version>/" (e.g. "openssl.org/v1.1.1w/bin/openssl"). The dist
// layout places each bottle at "<project>/<os>/<arch>/v<version><ext>" next to a
// "versions.txt" listing the published versions, one per line.
//
// Codec chooses the compression. It stays gzip until the readers are deployed:
// publishing a codec the installed base cannot read would brick every install,
// and a bottle already published is never rewritten. See Codec.
package bottlepkg

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"

	"github.com/go-pkgx/bottle"
	"github.com/klauspost/compress/zstd"
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

	gz, err := compressor(w)
	if err != nil {
		return err
	}
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
	tarballPath := filepath.Join(dir, "v"+version+Codec)
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

// Codec is the extension — and therefore the compression — new bottles are
// written with. One of bottle.ExtTarGz or bottle.ExtTarZst.
//
// It defaults to gzip, which is what the catalogue holds today, NOT to the
// codec that measures best. The order matters: a consumer that meets a bottle
// compressed with something it cannot decode fails with "invalid header", which
// reads like a corrupt download rather than a format it was never taught. So
// the readers ship first (bottle v0.8.0), then this is flipped, and only new
// publishes change — an already-published bottle is never rewritten.
//
// Measured on real bottle payloads, zstd -19 beats xz on ratio, compression
// time AND decompression time, and gzip has the poorest ratio of the three
// while decompressing slower than zstd. Every install pays the decompression;
// the factory pays the compression once.
var Codec = bottle.ExtTarGz

// compressor wraps w in the encoder Codec names. zstd is used at its highest
// level: the factory compresses a bottle once and every consumer pays for the
// size forever, so the asymmetry is worth the seconds.
func compressor(w io.Writer) (io.WriteCloser, error) {
	switch Codec {
	case bottle.ExtTarZst:
		return zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	case bottle.ExtTarGz:
		return gzip.NewWriter(w), nil
	default:
		return nil, fmt.Errorf("bottlepkg: unknown codec %q", Codec)
	}
}
