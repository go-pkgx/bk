package fetch

import (
	"archive/zip"
	"io"
	"net/http"
	"os"
	"os/exec"
)

// osWriteFlags is how extracted regular files are opened.
const osWriteFlags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC

// Injectable seams over the network, process and os operations whose error
// branches are otherwise unreachable in a test (a successful syscall that
// then fails is not reproducible with real files). Tests override these to
// exercise every error-handling path; production uses the real functions
// verbatim.
var (
	httpGet     = http.Get
	execCommand = exec.Command
	osMkdirAll  = os.MkdirAll
	osSymlink   = os.Symlink
	osOpenFile  = os.OpenFile
	ioCopy      = io.Copy
	zipOpen     = defaultZipOpen
)

// defaultZipOpen opens a file inside a zip archive for reading.
func defaultZipOpen(f *zip.File) (io.ReadCloser, error) {
	return f.Open()
}
