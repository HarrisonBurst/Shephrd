package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCleanAllRepinE2E(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	home := filepath.Join(dir, "home")
	commands := strings.Fields("shephrd shephrd-terminal-herdr shephrd-terminal-cmux shephrd-notification-macos shephrd-repository-scanner shephrd-github-observer shephrd-delivery-webhook")
	for _, path := range []string{"Makefile", "scripts/build-clean.sh"} {
		writeContextFixtureFile(t, root, path, readFile(t, filepath.Join("..", "..", path)))
	}
	if err := os.Chmod(filepath.Join(root, "scripts", "build-clean.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeContextFixtureFile(t, root, ".gitignore", "/bin/\n")
	writeContextFixtureFile(t, root, "go.mod", "module cleanbuildfixture\n\ngo 1.26.5\n")
	for _, command := range commands {
		writeContextFixtureFile(t, root, "cmd/"+command+"/main.go", "package main\nfunc main() {}\n")
	}
	contextGit(t, root, "init")
	contextGit(t, root, "add", ".")
	contextGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "clean build fixture")
	for _, command := range commands {
		writeContextFixtureFile(t, root, "bin/"+command, "installed sentinel\n")
	}
	oldPin := strings.Repeat("0", 64)
	config := filepath.Join(home, ".config", "shephrd", "config.toml")
	original := fmt.Sprintf("[terminal_extensions.herdr]\nsha256 = %q\n\n[terminal_extensions.cmux]\nsha256 = %q\n", oldPin, oldPin)
	writeContextFixtureFile(t, home, ".config/shephrd/config.toml", original)
	cache, err := exec.Command("go", "env", "GOCACHE", "GOPATH").Output()
	if err != nil {
		t.Fatal(err)
	}
	goPaths := strings.Split(strings.TrimSpace(string(cache)), "\n")
	run := func(target, repin string) (string, error) {
		t.Helper()
		command := exec.Command("make", target)
		command.Dir = root
		command.Env = compoundCLIEnvironment(os.Environ(), map[string]string{
			"HOME": home, "GOCACHE": goPaths[0], "GOPATH": goPaths[1], "SHEPHRD_BUILD_CLEAN_REPIN": repin,
		})
		output, err := command.CombinedOutput()
		return string(output), err
	}
	for _, repin := range []string{"", "true"} {
		output, err := run("build-clean-all", repin)
		if err == nil || !strings.Contains(output, "nothing was installed") || !strings.Contains(output, "  built  ") || !strings.Contains(output, "  pinned "+oldPin) {
			t.Fatalf("mismatch with opt-in %q: %s: %v", repin, output, err)
		}
		if got := readFile(t, config); got != original {
			t.Fatalf("refusal changed config: %s", got)
		}
		for _, command := range commands {
			if got := readFile(t, filepath.Join(root, "bin", command)); got != "installed sentinel\n" {
				t.Fatalf("refusal installed %s", command)
			}
		}
	}
	output, err := run("build-clean-all", "1")
	if err != nil {
		t.Fatalf("repin: %s: %v", output, err)
	}
	pin := fmt.Sprintf("%x", sha256.Sum256([]byte(readFile(t, filepath.Join(root, "bin", "shephrd-terminal-herdr")))))
	expected := strings.Replace(original, oldPin, pin, 1)
	if got := readFile(t, config); got != expected {
		t.Fatalf("repin changed more than the Herdr pin: %s", got)
	}
	if !strings.Contains(output, "repinned "+config+" to "+pin) || !strings.Contains(output, "sha256 to "+pin+" and run darwin-rebuild switch") {
		t.Fatalf("repin instructions disagree: %s", output)
	}
	for _, command := range commands {
		info, err := os.Stat(filepath.Join(root, "bin", command))
		if err != nil || info.Mode().Perm() != 0o755 || readFile(t, filepath.Join(root, "bin", command)) == "installed sentinel\n" {
			t.Fatalf("repin did not install executable %s: %v", command, err)
		}
	}
	before, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := run("build-clean-all", ""); err != nil || strings.Contains(output, "repinned") {
		t.Fatalf("matching pin: %s: %v", output, err)
	}
	after, err := os.Stat(config)
	if err != nil || !os.SameFile(before, after) || readFile(t, config) != expected {
		t.Fatalf("matching pin rewrote configuration: %v", err)
	}
	managed := filepath.Join(home, "managed.toml")
	writeContextFixtureFile(t, home, "managed.toml", original)
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(managed, config); err != nil {
		t.Fatal(err)
	}
	if output, err := run("build-clean", "1"); err != nil || readFile(t, config) != original {
		t.Fatalf("CLI-only build changed pin: %s: %v", output, err)
	}
	if output, err := run("build-clean-all", "1"); err != nil {
		t.Fatalf("symlink repin: %s: %v", output, err)
	}
	if readFile(t, config) != expected || readFile(t, managed) != original {
		t.Fatal("symlink repin changed the managed source or missed the live pin")
	}
	if files, err := filepath.Glob(config + ".*"); err != nil || len(files) != 0 {
		t.Fatalf("repin left staging files: %v: %v", files, err)
	}
}
