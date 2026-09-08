package fetch

import (
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

// hex64 matches a lowercase-or-uppercase SHA-256 in hex.
var hex64 = regexp.MustCompile(`(?i)\b[0-9a-f]{64}\b`)

// maxChecksumBody caps how much of a checksum file is read. A SHA256SUMS for a
// large release is a few kilobytes; anything past this is not one, and reading
// it all would turn a wrong URL into a memory problem.
const maxChecksumBody = 1 << 20

// DeclaredSHA256 fetches a checksum file and returns the SHA-256 it declares
// for archiveName, in lowercase hex.
//
// The pantry format already had this: `sha: ${{url}}.sha256` points at what the
// upstream publishes NEXT to the tarball, so the expected digest moves with the
// version instead of being re-written into the recipe for each one. bk simply
// never read it.
//
// Three shapes are in the wild and all three appear in this ecosystem:
//
//	b6a5f44b…                          bare hex (openssl.org)
//	b6a5f44b…  openssl-3.6.0.tar.gz    coreutils `sha256sum` output
//	SHA256 (openssl-3.6.0.tar.gz) = b6a5f44b…   BSD `sha256 -r` output
//
// A file listing MANY archives is matched on the basename, because taking the
// first line of a SHA256SUMS covering a whole release would verify the download
// against some other file's digest and call it a pass.
func DeclaredSHA256(url, archiveName string) (string, error) {
	resp, err := httpGet(url)
	if err != nil {
		return "", fmt.Errorf("fetch: get checksum %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("fetch: get checksum %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumBody))
	if err != nil {
		return "", fmt.Errorf("fetch: read checksum %s: %w", url, err)
	}
	sum, err := parseChecksum(string(body), archiveName)
	if err != nil {
		return "", fmt.Errorf("fetch: checksum %s: %w — %s", url, err, describeBody(body))
	}
	return sum, nil
}

// parseChecksum extracts the digest for name from a checksum file's text.
func parseChecksum(body, name string) (string, error) {
	var lines []string
	for _, ln := range strings.Split(body, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("empty")
	}
	// One line and one digest on it: nothing to disambiguate, whatever the
	// surrounding syntax. This is the openssl.org and the single-file
	// sha256sum case at once.
	if len(lines) == 1 {
		if m := hex64.FindString(lines[0]); m != "" {
			return strings.ToLower(m), nil
		}
		return "", fmt.Errorf("no sha-256 found")
	}
	base := path.Base(name)
	for _, ln := range lines {
		m := hex64.FindString(ln)
		if m == "" {
			continue
		}
		// The rest of the line names the file, in either layout. Compare on the
		// basename: a SHA256SUMS may name "./foo.tar.gz" or "(foo.tar.gz)".
		rest := strings.NewReplacer("(", " ", ")", " ", "=", " ", "*", " ").Replace(strings.Replace(ln, m, " ", 1))
		for _, f := range strings.Fields(rest) {
			if path.Base(f) == base {
				return strings.ToLower(m), nil
			}
		}
	}
	return "", fmt.Errorf("no line names %s", base)
}
