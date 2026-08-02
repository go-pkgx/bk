package build

import (
	"os"
	"time"
)

// osChtimes is a seam so TouchAutotools' error branch is testable.
var osChtimes = func(name string, atime, mtime time.Time) error {
	return os.Chtimes(name, atime, mtime)
}

// Injectable os seams for the Runner's glue, so every error branch is testable.
var (
	osRemoveAll = os.RemoveAll
	osMkdirAll  = os.MkdirAll
	osWriteFile = os.WriteFile
	osRename    = os.Rename
)
