package build

import (
	"os"
	"time"
)

// osChtimes is a seam so TouchAutotools' error branch is testable.
var osChtimes = func(name string, atime, mtime time.Time) error {
	return os.Chtimes(name, atime, mtime)
}
