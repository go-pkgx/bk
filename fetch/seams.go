package fetch

import (
	"archive/zip"
	"io"
	"net/http"
	"os"

	gogit "github.com/go-git/go-git/v5"
)

// osWriteFlags is how extracted regular files are opened.
const osWriteFlags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC

// Injectable seams over the network, process and os operations whose error
// branches are otherwise unreachable in a test (a successful syscall that
// then fails is not reproducible with real files). Tests override these to
// exercise every error-handling path; production uses the real functions
// verbatim.
var (
	httpGet       = http.Get
	gitPlainClone = gogit.PlainClone
	osRemoveAll   = os.RemoveAll
	osMkdirAll    = os.MkdirAll
	osOpen        = os.Open
	osSymlink     = os.Symlink
	osOpenFile    = os.OpenFile
	osChtimes     = os.Chtimes
	ioCopy        = io.Copy
	zipOpen       = defaultZipOpen
)

// defaultZipOpen opens a file inside a zip archive for reading.
func defaultZipOpen(f *zip.File) (io.ReadCloser, error) {
	return f.Open()
}
