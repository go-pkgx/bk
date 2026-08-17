package fetch

import (
	"fmt"
	"net/http"
)

// downloadAttempts is how many times a truncated download is resumed before the
// fetch is called a failure. A source tarball is fetched once per build, so a
// handful of attempts costs nothing and can save the whole build.
const downloadAttempts = 5

// download streams url into a temporary file and returns its path. A body that
// stops early is RESUMED with a Range request rather than restarted, and the
// result is checked against the length the server announced.
//
// Streaming a response straight into the tar extractor — as this package used
// to — cannot recover from a truncated transfer: the extractor has already
// consumed and written part of the archive when the read fails, and the only
// signal is `unexpected EOF`. That is exactly how kernel.org/linux failed: a
// ~150 MB xz tarball, the largest source we fetch, cut mid-transfer. bottle
// learned the same lesson on bottle blobs (Range resume + retries); this is the
// source-side half of it.
func download(url string) (string, error) {
	f, err := osCreateTemp("", "bk-fetch-*")
	if err != nil {
		return "", fmt.Errorf("fetch: temp file: %w", err)
	}
	path := f.Name()
	var got, want int64

	for attempt := 0; attempt < downloadAttempts; attempt++ {
		resp, err := httpGetRange(url, got)
		if err != nil {
			f.Close()
			osRemove(path)
			return "", fmt.Errorf("fetch: GET %s: %w", url, err)
		}
		switch resp.StatusCode {
		case http.StatusPartialContent:
			// The server honoured the Range: keep appending to what we have.
		case http.StatusOK:
			// It ignored the Range and is replaying the whole body, so start over
			// on a fresh file rather than append to a partial one.
			if got > 0 {
				f.Close()
				osRemove(path)
				nf, err := osCreateTemp("", "bk-fetch-*")
				if err != nil {
					resp.Body.Close()
					return "", fmt.Errorf("fetch: temp file: %w", err)
				}
				f, path, got, want = nf, nf.Name(), 0, 0
			}
		default:
			resp.Body.Close()
			f.Close()
			osRemove(path)
			return "", fmt.Errorf("fetch: GET %s: %s", url, resp.Status)
		}
		if resp.ContentLength > 0 {
			want = got + resp.ContentLength
		}

		n, copyErr := ioCopyBody(f, resp.Body)
		resp.Body.Close()
		got += n
		if copyErr == nil && (want == 0 || got >= want) {
			f.Close()
			return path, nil
		}
		// Truncated: loop and resume from exactly where this attempt stopped.
	}

	f.Close()
	osRemove(path)
	return "", fmt.Errorf("fetch: GET %s: still truncated after %d attempts (%d of %d bytes)",
		url, downloadAttempts, got, want)
}

// httpGetRange requests url, asking for everything from offset on when a
// previous attempt already stored some bytes.
func httpGetRange(url string, offset int64) (*http.Response, error) {
	if offset == 0 {
		return httpGet(url)
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	return httpDo(req)
}
