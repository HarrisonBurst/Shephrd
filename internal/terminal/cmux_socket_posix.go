package terminal

import (
	"os"
	"syscall"
)

func cmuxSocketOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
