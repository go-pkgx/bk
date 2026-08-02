package bottlepkg

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
)

// Injectable seams over the operations whose error branches are otherwise
// unreachable in a test (a successful syscall that then fails is not
// reproducible with real files). Tests override these to exercise every
// error-handling path; production uses the stdlib functions verbatim.
var (
	walkDir           = filepath.WalkDir
	osLstat           = os.Lstat
	osReadlink        = os.Readlink
	osOpen            = os.Open
	osMkdirAll        = os.MkdirAll
	ioCopy            = io.Copy
	tarFileInfoHeader = tar.FileInfoHeader

	// osCreate and osOpenFileAppend return io.WriteCloser (not *os.File) so
	// tests can inject writers whose Write or Close fails.
	osCreate = func(name string) (io.WriteCloser, error) {
		return os.Create(name)
	}
	osOpenFileAppend = func(name string) (io.WriteCloser, error) {
		return os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	}
)
