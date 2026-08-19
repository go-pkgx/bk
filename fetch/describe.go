package fetch

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// sniffLen is how much of a downloaded file describeBody looks at. Enough to
// recognise a redirect page or an error message, short enough to quote.
const sniffLen = 256

// describeBody says what a download actually contains, for an error message
// that would otherwise blame the format.
//
// A mirror that answers 200 with an HTML error page, a login redirect, or a
// plain-text "not found" produces exactly one symptom:
//
//	fetch: read gzip from https://zlib.net/zlib-1.3.2.tar.gz: gzip: invalid header
//
// which reads as a corrupt archive and sends the reader to check the tarball.
// The archive was never there. Quoting the first bytes turns a mystery into a
// diagnosis, and costs nothing on the path where everything worked.
func describeBody(head []byte) string {
	if len(head) == 0 {
		return "the server returned an EMPTY body"
	}
	if isMostlyText(head) {
		snippet := strings.TrimSpace(string(head))
		if len(snippet) > 160 {
			snippet = snippet[:160] + "…"
		}
		snippet = strings.Join(strings.Fields(snippet), " ")
		kind := "text"
		lower := strings.ToLower(snippet)
		switch {
		case strings.HasPrefix(lower, "<!doctype html"), strings.HasPrefix(lower, "<html"):
			kind = "an HTML page"
		case strings.HasPrefix(lower, "<?xml"):
			kind = "an XML document"
		}
		return fmt.Sprintf("the server returned %s, not an archive: %q", kind, snippet)
	}
	return fmt.Sprintf("the body does not start like the expected archive (first bytes: % x)", head[:min(8, len(head))])
}

// isMostlyText reports whether b looks like human-readable text rather than a
// compressed stream. Compressed data is high-entropy bytes; an error page is
// not, and that difference is the whole signal.
func isMostlyText(b []byte) bool {
	printable := 0
	total := 0
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			total++
			continue
		}
		i += size
		total++
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			printable++
		}
	}
	if total == 0 {
		return false
	}
	return printable*10 >= total*9 // 90% printable
}

// readHead reads the first sniffLen bytes of f and rewinds it, so the caller's
// decompressor still sees a stream positioned at the start.
func readHead(f readSeeker) []byte {
	buf := make([]byte, sniffLen)
	n, _ := f.Read(buf)
	if _, err := f.Seek(0, 0); err != nil {
		return nil
	}
	return buf[:n]
}

// readSeeker is the slice of *os.File readHead needs.
type readSeeker interface {
	Read([]byte) (int, error)
	Seek(int64, int) (int64, error)
}
