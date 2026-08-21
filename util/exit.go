package util

import "os"

var OsExit = os.Exit

// Exit codes eRPC returns to its supervisor.
//
// A Unix exit status is ONE BYTE: the kernel reports code & 0xFF, so a
// value above 255 reaches the shell as a different number (1001 arrived
// as 233). Keep every code in 2..125 — 0 means success, 1 means "bad
// config" for `validate` and `dump`, 126 and 127 belong to the shell, and
// 128+n means "killed by signal n".
var (
	ExitCodeERPCStartFailed  = 11
	ExitCodeHttpServerFailed = 12
)
