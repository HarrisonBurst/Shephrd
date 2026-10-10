package execution

import (
	"os"
	"path/filepath"
	"strings"

	"shephrd/internal/fault"
	"shephrd/internal/gitcmd"
)

const MaxReportFile = 10 << 20

type SealRequest struct {
	Deliverable string   `json:"deliverable"`
	Workspace   string   `json:"workspace"`
	Branch      string   `json:"branch"`
	Base        string   `json:"base"`
	Files       []string `json:"files"`
}

type SealedFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

type SealResponse struct {
	Commit string       `json:"commit,omitempty"`
	Files  []SealedFile `json:"files,omitempty"`
}

func refuseResult(format string, args ...any) error {
	return fault.New("result_refused", format, args...)
}

// Seal checks a worker's result in its workspace: committed code on the
// attempt branch beyond its base, or regular report files inside the
// workspace, whose contents it returns for the home host to store.
func Seal(req SealRequest) (*SealResponse, error) {
	switch req.Deliverable {
	case "code":
		if len(req.Files) > 0 {
			return nil, refuseResult("a code result is the committed branch; --file is for report results")
		}
		status, err := gitcmd.Run(req.Workspace, "status", "--porcelain")
		if err != nil {
			return nil, err
		}
		if status != "" {
			return nil, refuseResult("the workspace has uncommitted changes; commit everything on %s and report again:\n%s", req.Branch, status)
		}
		if head, err := gitcmd.Run(req.Workspace, "symbolic-ref", "--short", "HEAD"); err != nil || head != req.Branch {
			return nil, refuseResult("HEAD must be on %s", req.Branch)
		}
		commit, err := gitcmd.Run(req.Workspace, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		if count, err := gitcmd.Run(req.Workspace, "rev-list", "--count", req.Base+"..HEAD"); err != nil || count == "0" {
			return nil, refuseResult("there are no commits beyond the base %s", req.Base)
		}
		return &SealResponse{Commit: commit}, nil
	case "report":
		if len(req.Files) == 0 {
			return nil, refuseResult("a report result names its documents with --file <path>")
		}
		out := &SealResponse{}
		for _, name := range req.Files {
			file, err := readReportFile(req.Workspace, name)
			if err != nil {
				return nil, err
			}
			out.Files = append(out.Files, file)
		}
		return out, nil
	}
	return &SealResponse{}, nil
}

func readReportFile(workspace, name string) (SealedFile, error) {
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, name)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return SealedFile{}, refuseResult("report file %s does not exist", name)
	}
	if !strings.HasPrefix(resolved, workspace+string(filepath.Separator)) {
		return SealedFile{}, refuseResult("report file %s is outside the workspace", name)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return SealedFile{}, refuseResult("report file %s is not a regular file", name)
	}
	if info.Size() > MaxReportFile {
		return SealedFile{}, refuseResult("report file %s is larger than %d bytes", name, MaxReportFile)
	}
	body, err := os.ReadFile(resolved)
	if err != nil {
		return SealedFile{}, err
	}
	rel, _ := filepath.Rel(workspace, resolved)
	return SealedFile{Path: rel, Content: body}, nil
}
