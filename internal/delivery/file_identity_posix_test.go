package delivery

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPOSIXFileIdentityUsesDeviceAndInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(path, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("file stat has no POSIX identity")
	}
	want := fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	if got := fileIdentity(file, info); got != want {
		t.Fatalf("file identity = %q, want %q", got, want)
	}
}
