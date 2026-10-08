package terminal

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPOSIXCmuxSocketRequiresCurrentOwner(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "shephrd-cmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "cmux.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cmuxSocketOwned(info) {
		t.Fatal("current-user socket was not recognized as owned")
	}
	if err := validateCmuxSocket(path); err != nil {
		t.Fatal(err)
	}
}
