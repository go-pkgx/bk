package main

import (
	"fmt"
	"io"
	"os"

	"github.com/go-pkgx/bk/fetch"
	"github.com/go-pkgx/bottle"
)

// sourceMirrorProject is the single repository the mirror lives in.
//
// One repository, not one per project, because the store is addressed by
// CONTENT: the tag is the archive's sha256, so which project happened to fetch
// it first is not part of its identity. Two recipes that build from the same
// tarball share one object rather than two.
const sourceMirrorProject = "sources"

// newSourceStore is a seam: the tests drive the hook without a registry.
var newSourceStore = func(base string) (sourceStore, error) {
	c, err := bottle.NewOCIClient(base)
	if err != nil {
		return nil, err
	}
	return ociSourceStore{c}, nil
}

// sourceStore is the half of bottle's source API this needs.
type sourceStore interface {
	HasSource(project, sha256hex string) (bool, error)
	PushSource(project string, data []byte, uri string) error
}

type ociSourceStore struct{ c *bottle.OCIClient }

func (s ociSourceStore) HasSource(project, sha string) (bool, error) {
	return s.c.HasSource(project, sha)
}

func (s ociSourceStore) PushSource(project string, data []byte, uri string) error {
	_, err := s.c.PushSource(project, data, uri)
	return err
}

// installSourceMirror points fetch.Mirror at a source store, so every archive a
// build downloads is kept.
//
// Populating only. Consuming it — fetching FROM the mirror when a recipe
// declares a digest — is a separate change, and this one has to run first: a
// store nobody has written to is not worth reading.
//
// Every failure here is reported and swallowed. A build that already holds the
// bytes it needs must not die because a registry was unreachable; the mirror
// serves the next rebuild, not this one. Silence would be the wrong choice for
// the same reason it is everywhere else in this factory — a mirror that quietly
// stopped recording would look exactly like one that had nothing to record.
func installSourceMirror(base string, stderr io.Writer) error {
	store, err := newSourceStore(base)
	if err != nil {
		return fmt.Errorf("source mirror %s: %w", base, err)
	}
	fetch.Mirror = func(path, sha, url string) {
		switch have, err := store.HasSource(sourceMirrorProject, sha); {
		case err != nil:
			fmt.Fprintf(stderr, "source mirror: checking %s: %v\n", sha, err)
			return
		case have:
			// The tag IS the content, so this is not an optimisation: pushing
			// again would write the same manifest. It is megabytes not spent.
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "source mirror: reading %s: %v\n", path, err)
			return
		}
		if err := store.PushSource(sourceMirrorProject, data, url); err != nil {
			fmt.Fprintf(stderr, "source mirror: storing %s (%s): %v\n", sha, url, err)
			return
		}
		fmt.Fprintf(stderr, "source mirror: stored %s (%d KiB) from %s\n", sha, len(data)/1024, url)
	}
	return nil
}
