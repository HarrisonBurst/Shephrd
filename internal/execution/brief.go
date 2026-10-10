package execution

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"shephrd/internal/coord"
	"shephrd/internal/store"
)

const (
	maxBrief = 64 << 10
	maxEntry = 4 << 10
)

type briefInput struct {
	shephrd   string
	task      *coord.Task
	attempt   *coord.Attempt
	run       *coord.Run
	repoPath  string
	nudge     string
	guide     string
	since     []store.Event
	notes     []store.Event
	requests  []requestView
	inputs    []string
	truncated bool
}

type requestView struct {
	coord.Request
	body     string
	children []*coord.Task
}

func renderBrief(in briefInput) string {
	t := in.task
	var b strings.Builder
	fmt.Fprintf(&b, "# Shephrd brief: %s, attempt %d, run %d\n\n", t.Ref, in.attempt.N, in.run.Generation)
	role := "a **worker**"
	if t.Role == "driver" {
		role = "a **sub-driver**"
	}
	where := "with no repository"
	if t.Repo != "" {
		where = "in repository " + t.Repo
	}
	fmt.Fprintf(&b, "You are %s on task %s %s. Report with `shephrd report`; your identity comes from your environment.\n\n", role, t.Ref, where)
	fmt.Fprintf(&b, "Run Shephrd as `%s`. Another `shephrd` on your PATH may be a different install that does not know this task. Any Shephrd skill installed in your harness is for the main driver, not for you: follow this brief.\n\n", in.shephrd)
	if in.nudge != "" {
		fmt.Fprintf(&b, "**Your previous run ended without reporting (%s).** Finish the turn now: report a result, question or blocker", in.nudge)
		if t.Role == "driver" {
			b.WriteString(", then a closing checkpoint note")
		}
		b.WriteString(" with `shephrd report`.\n\n")
	}
	b.WriteString("## Task\n\n")
	fmt.Fprintf(&b, "**Title:** %s\n\n**Objective:**\n\n%s\n\n", t.Title, t.Objective)
	if t.Acceptance != "" {
		fmt.Fprintf(&b, "**Acceptance:**\n\n%s\n\n", t.Acceptance)
	}
	fmt.Fprintf(&b, "**Deliverable:** %s. %s\n\n", t.Deliverable, deliverableHint(t, in.attempt))
	b.WriteString("## Workspace\n\n")
	fmt.Fprintf(&b, "- Path: `%s`\n", in.attempt.Workspace)
	if in.attempt.Branch != "" {
		fmt.Fprintf(&b, "- Branch: `%s`, from base `%s`\n", in.attempt.Branch, in.attempt.Base)
	}
	if t.Role == "driver" {
		b.WriteString("- The workspace is read-only. Workers make every change.\n")
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", filepath.Join(".shephrd", "context.md")} {
		if _, err := os.Stat(filepath.Join(in.attempt.Workspace, name)); err == nil {
			fmt.Fprintf(&b, "- Repository guidance: `%s`\n", name)
		}
	}
	b.WriteString("\n")
	if len(in.inputs) > 0 {
		b.WriteString("## Inputs\n\nResults of the tasks this one depends on, pinned when it started:\n\n")
		for _, input := range in.inputs {
			fmt.Fprintf(&b, "- %s\n", input)
		}
		b.WriteString("\n")
	}
	if len(in.requests) > 0 {
		b.WriteString("## Open requests\n\n")
		for _, r := range in.requests {
			fmt.Fprintf(&b, "### Request %d (%s)\n\n%s\n\n", r.Seq, r.State, excerpt(r.body))
			for _, child := range r.children {
				fmt.Fprintf(&b, "- %s %s %s: %s", child.Ref, child.Role, child.State, child.Title)
				if child.Reason != "" {
					fmt.Fprintf(&b, " (%s)", child.Reason)
				}
				b.WriteString("\n")
			}
			if len(r.children) > 0 {
				b.WriteString("\n")
			}
		}
	}
	if len(in.since) > 0 {
		b.WriteString("## Since your last turn\n\n")
		for _, e := range in.since {
			fmt.Fprintf(&b, "- [%d] %s on %s from %s: %s\n", e.Seq, e.Name, e.Task, e.Caller, excerpt(eventText(e)))
		}
		if in.truncated {
			fmt.Fprintf(&b, "- More events: `shephrd task show %s`\n", t.Ref)
		}
		b.WriteString("\n")
	}
	if len(in.notes) > 0 {
		b.WriteString("## Notes\n\n")
		for _, e := range in.notes {
			fmt.Fprintf(&b, "- [%d] from %s: %s\n", e.Seq, e.Caller, excerpt(eventText(e)))
		}
		b.WriteString("\n")
	}
	b.WriteString("## Guidance\n\n")
	b.WriteString(in.guide)
	out := b.String()
	if len(out) > maxBrief {
		out = out[:maxBrief-200] + fmt.Sprintf("\n\n[Brief truncated. Read the rest with `shephrd task show %s`.]\n", t.Ref)
	}
	return out
}

func deliverableHint(t *coord.Task, a *coord.Attempt) string {
	switch {
	case t.Role == "driver":
		return "Report each request's result with `shephrd report result - --request <seq>`."
	case t.Deliverable == "code":
		return fmt.Sprintf("Commit your work on `%s` and leave a clean tree before `shephrd report result`.", a.Branch)
	default:
		return "Write the report as files and name each with `--file <path>` on `shephrd report result`."
	}
}

func eventText(e store.Event) string {
	var data map[string]any
	json.Unmarshal(e.Data, &data)
	if body, ok := data["body"].(string); ok {
		return body
	}
	return string(e.Data)
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxEntry {
		return s[:maxEntry] + " [...]"
	}
	return s
}
