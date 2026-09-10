package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bk/fetch"
)

type fakeStore struct {
	have    map[string]bool
	pushed  map[string][]byte
	uris    map[string]string
	hasErr  error
	pushErr error
}

func (f *fakeStore) HasSource(_, sha string) (bool, error) {
	if f.hasErr != nil {
		return false, f.hasErr
	}
	return f.have[sha], nil
}

func (f *fakeStore) PushSource(_ string, data []byte, uri string) error {
	if f.pushErr != nil {
		return f.pushErr
	}
	sha := "sha-of-" + string(data)
	if f.pushed == nil {
		f.pushed, f.uris = map[string][]byte{}, map[string]string{}
	}
	f.pushed[sha] = data
	f.uris[sha] = uri
	return nil
}

func withStore(t *testing.T, s *fakeStore) *bytes.Buffer {
	t.Helper()
	old := newSourceStore
	newSourceStore = func(string) (sourceStore, error) { return s, nil }
	var log bytes.Buffer
	if err := installSourceMirror("oci://example.invalid/go-pkgx", &log); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { newSourceStore = old; fetch.Mirror = nil })
	return &log
}

func archive(t *testing.T, body string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.tar.gz")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, "deadbeef"
}

// The happy path: an archive nobody has stored gets stored, with the URL it
// came from recorded beside it as provenance.
func TestSourceMirrorStoresWhatIsNew(t *testing.T) {
	s := &fakeStore{have: map[string]bool{}}
	log := withStore(t, s)
	p, sha := archive(t, "the bytes")

	fetch.Mirror(p, sha, "https://example.invalid/thing.tar.gz")

	if string(s.pushed["sha-of-the bytes"]) != "the bytes" {
		t.Errorf("stored %q", s.pushed)
	}
	if s.uris["sha-of-the bytes"] != "https://example.invalid/thing.tar.gz" {
		t.Errorf("uri = %q", s.uris)
	}
	if !strings.Contains(log.String(), "stored "+sha) {
		t.Errorf("log = %q, want it to say what it kept", log.String())
	}
}

// The tag IS the content, so re-pushing writes the same manifest. Skipping is
// not an optimisation of correctness — it is megabytes not spent, on every
// build of every package that shares a tarball.
func TestSourceMirrorSkipsWhatItAlreadyHas(t *testing.T) {
	s := &fakeStore{have: map[string]bool{"deadbeef": true}}
	withStore(t, s)
	p, sha := archive(t, "the bytes")

	fetch.Mirror(p, sha, "https://example.invalid/thing.tar.gz")

	if len(s.pushed) != 0 {
		t.Errorf("re-uploaded an archive the store already had: %v", s.pushed)
	}
}

// Every failure is reported and swallowed. A build holding the bytes it needs
// must not die because a registry was unreachable — but a mirror that quietly
// stopped recording would look exactly like one with nothing to record, so it
// says so.
func TestSourceMirrorReportsAndSwallows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store *fakeStore
		path  string
		want  string
	}{
		{"the check fails", &fakeStore{hasErr: errors.New("boom")}, "", "checking"},
		{"the push fails", &fakeStore{have: map[string]bool{}, pushErr: errors.New("boom")}, "", "storing"},
		{"the archive is gone", &fakeStore{have: map[string]bool{}}, "/nonexistent/src.tar.gz", "reading"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := withStore(t, tc.store)
			p, sha := archive(t, "the bytes")
			if tc.path != "" {
				p = tc.path
			}
			fetch.Mirror(p, sha, "https://example.invalid/thing.tar.gz") // must not panic or exit
			if !strings.Contains(log.String(), tc.want) {
				t.Errorf("log = %q, want it to mention %q", log.String(), tc.want)
			}
		})
	}
}

// A store that cannot be constructed at all is a startup error, not a silent
// no-op: the operator asked for a mirror and is entitled to know they have none.
func TestInstallSourceMirrorReportsAConstructionFailure(t *testing.T) {
	old := newSourceStore
	newSourceStore = func(string) (sourceStore, error) { return nil, errors.New("bad base") }
	defer func() { newSourceStore = old; fetch.Mirror = nil }()
	if err := installSourceMirror("nonsense", io.Discard); err == nil {
		t.Error("installSourceMirror accepted a store it could not build")
	}
}

// The adapters are two lines each, and two lines each is where a wrong argument
// order hides. Driven against a base that resolves to nothing, so both report
// rather than pretending.
func TestOCISourceStoreAdapters(t *testing.T) {
	s, err := newSourceStore("oci://127.0.0.1:1/go-pkgx")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.HasSource(sourceMirrorProject, "deadbeef"); err == nil {
		t.Error("HasSource reported an answer from a registry that is not there")
	}
	if err := s.PushSource(sourceMirrorProject, []byte("x"), "https://example.invalid/x"); err == nil {
		t.Error("PushSource reported success against a registry that is not there")
	}
}

// An operator who asked for a mirror and cannot have one is told before the
// build starts, not after a hundred archives have gone unrecorded.
func TestFactoryRefusesAMirrorItCannotBuild(t *testing.T) {
	old := newSourceStore
	newSourceStore = func(string) (sourceStore, error) { return nil, errors.New("bad base") }
	defer func() { newSourceStore = old; fetch.Mirror = nil }()
	var out, errOut bytes.Buffer
	if code := runFactory([]string{"--platform", "linux/x86-64", "--source-mirror", "nonsense", "acme.org/thing"}, &out, &errOut); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "source mirror") {
		t.Errorf("stderr = %q, want it to name the source mirror", errOut.String())
	}
}
