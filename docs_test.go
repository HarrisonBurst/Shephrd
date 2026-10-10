package shephrd

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var markdownLinkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

func TestDocumentation(t *testing.T) {
	root := "."
	files := markdownFiles(t, root)

	t.Run("internal links", func(t *testing.T) {
		for _, path := range files {
			body := readDocumentationFile(t, path)
			for _, match := range markdownLinkPattern.FindAllStringSubmatch(body, -1) {
				target := strings.Trim(match[1], "<>")
				if target == "" || strings.HasPrefix(target, "#") || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				target, _, _ = strings.Cut(target, "#")
				target, _, _ = strings.Cut(target, "?")
				resolved := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target)))
				if _, err := os.Stat(resolved); err != nil {
					t.Errorf("%s links to missing %s", relativePath(root, path), target)
				}
			}
		}
	})

	t.Run("mermaid fences", func(t *testing.T) {
		for _, path := range files {
			inMermaid := false
			for lineNumber, line := range strings.Split(readDocumentationFile(t, path), "\n") {
				switch strings.TrimSpace(line) {
				case "```mermaid":
					if inMermaid {
						t.Errorf("%s:%d nests a Mermaid fence", relativePath(root, path), lineNumber+1)
					}
					inMermaid = true
				case "```":
					if inMermaid {
						inMermaid = false
					}
				}
			}
			if inMermaid {
				t.Errorf("%s has an unclosed Mermaid fence", relativePath(root, path))
			}
		}
	})
}

func markdownFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && filepath.Ext(path) == ".md" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func readDocumentationFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func relativePath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}
