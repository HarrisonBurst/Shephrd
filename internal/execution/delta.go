package execution

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"shephrd/internal/fault"
	"shephrd/internal/gitcmd"
)

var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func ProveDelta(req DeltaRequest) (*DeltaProof, error) {
	unproven := func() error { return fault.New("not_proven", "the sealed delta is not the recorded merge carrier") }
	for _, id := range []string{req.Base, req.Commit, req.Carrier} {
		if !objectID.MatchString(id) {
			return nil, unproven()
		}
	}
	tip, err := branchTip(req.Repo, req.Branch)
	if err != nil {
		return nil, err
	}
	if !isAncestor(req.Repo, req.Carrier, tip) {
		return nil, unproven()
	}
	parents, err := gitcmd.Run(req.Repo, "rev-list", "--parents", "-n", "1", req.Carrier)
	if err != nil || len(strings.Fields(parents)) != 2 || strings.Fields(parents)[0] != req.Carrier {
		return nil, unproven()
	}
	premerge := strings.Fields(parents)[1]
	baseTree, err := gitcmd.Run(req.Repo, "rev-parse", req.Base+"^{tree}")
	if err != nil {
		return nil, unproven()
	}
	sealedTree, err := gitcmd.Run(req.Repo, "rev-parse", req.Commit+"^{tree}")
	if err != nil || sealedTree == baseTree {
		return nil, unproven()
	}
	carrierTree, err := gitcmd.Run(req.Repo, "rev-parse", req.Carrier+"^{tree}")
	if err != nil {
		return nil, unproven()
	}
	dir, err := os.MkdirTemp("", "shephrd-delta-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	index := filepath.Join(dir, "index")
	run := func(input []byte, args ...string) ([]byte, error) {
		cmd := exec.Command("git", append([]string{"-C", req.Repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
		cmd.Stdin = bytes.NewReader(input)
		return cmd.Output()
	}
	patch, err := run(nil, "diff", "--binary", "--full-index", req.Base, req.Commit)
	if err != nil {
		return nil, unproven()
	}
	if _, err := run(nil, "read-tree", premerge); err != nil {
		return nil, unproven()
	}
	if _, err := run(patch, "apply", "--cached", "-"); err != nil {
		return nil, unproven()
	}
	result, err := run(nil, "write-tree")
	if err != nil {
		return nil, unproven()
	}
	proof := &DeltaProof{Premerge: premerge, ExpectedTree: strings.TrimSpace(string(result)), CarrierTree: carrierTree}
	if proof.ExpectedTree != proof.CarrierTree {
		return nil, unproven()
	}
	return proof, nil
}
