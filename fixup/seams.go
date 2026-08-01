package fixup

import "os"

// Injectable seams over the os operations whose error branches are otherwise
// unreachable in a test (a successful syscall that then fails is not
// reproducible with real files). Tests override these to exercise every
// error-handling path; production uses the os functions verbatim.
var (
	osStat      = os.Stat
	osReadDir   = os.ReadDir
	osWriteFile = os.WriteFile
	osRename    = os.Rename
	osRemove    = os.Remove
	osSymlink   = os.Symlink
	osMkdirAll  = os.MkdirAll
	osOpenFile  = os.OpenFile
)
