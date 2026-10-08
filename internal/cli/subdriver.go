package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"shephrd/internal/control"
	"shephrd/internal/model"
)

func subdriverCommand(get func() *application) *cobra.Command {
	command := commandGroup("subdriver", "Opt-in sub-drivers: durable repository and general coordination")
	command.Aliases = []string{"coordinator"}
	print := func(value any) error {
		return get().print(value, func() { body, _ := json.MarshalIndent(value, "", "  "); fmt.Fprintln(get().out, string(body)) })
	}
	var listDriver string
	var listAll bool
	var listOffset int
	list := &cobra.Command{Use: "ls", Short: "Discover durable owners without adopting requests or workers", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		owner := ""
		var err error
		if listAll && cmd.Flags().Changed("driver-id") {
			return fmt.Errorf("choose --all-drivers or --driver-id")
		}
		if !listAll {
			owner, err = resolveDriverID(listDriver, cmd.Flags().Changed("driver-id"), true)
			if err != nil {
				return err
			}
		}
		cs, err := get().store.Subdrivers(owner, listOffset)
		if err != nil {
			return err
		}
		next := ""
		if len(cs) > 20 {
			cs = cs[:20]
			scope := "--all-drivers"
			if owner != "" {
				scope = "--driver-id " + model.ShellQuote(owner)
			}
			next = fmt.Sprintf("shephrd subdriver ls %s --offset %d --json", scope, listOffset+20)
		}
		return print(struct {
			Subdrivers []model.Subdriver `json:"subdrivers"`
			Legacy     []model.Subdriver `json:"coordinators"`
			Next       string            `json:"next,omitempty"`
		}{cs, cs, next})
	}}
	list.Flags().StringVar(&listDriver, "driver-id", "", "Current main return owner")
	list.Flags().BoolVar(&listAll, "all-drivers", false, "Read-only discovery across main return owners")
	list.Flags().IntVar(&listOffset, "offset", 0, "Page offset")
	command.AddCommand(list)
	var repo, contextText, general, driver, key, lead, file string
	var queue bool
	handoff := &cobra.Command{Use: "handoff [original-request]", Short: "Forward an original request; start a bounded sub-driver session unless queued", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if (repo == "") == (general == "") {
			return fmt.Errorf("select --repo or explicit --general-context, not both")
		}
		if (len(args) == 0) == (file == "") {
			return fmt.Errorf("provide original request as one argument or --request-file")
		}
		original := ""
		if file != "" {
			body, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			original = string(body)
		} else {
			original = args[0]
		}
		owner, err := resolveDriverID(driver, cmd.Flags().Changed("driver-id"), true)
		if err != nil {
			return err
		}
		repoID := ""
		if repo != "" {
			r, err := get().store.Repo(repo)
			if err != nil {
				return err
			}
			repoID = r.ID
		}
		request, err := get().store.HandoffSubdriver(repoID, general, owner, key, original, contextText, lead)
		if err != nil {
			return err
		}
		if !queue {
			c, err := get().store.Subdriver(request.SubdriverID)
			if err != nil {
				return err
			}
			if c.State == "idle" {
				if _, err = get().control.ResumeSubdriver(c.ID, false); err != nil {
					return fmt.Errorf("request %s persisted; sub-driver %s requires resume: %w", request.ID, c.ID, err)
				}
			}
		}
		return print(request)
	}}
	handoff.Flags().StringVar(&repo, "repo", "", "Existing registered repo name or ID")
	handoff.Flags().StringVar(&general, "general-context", "", "Explicit context for a new on-demand general/research owner")
	handoff.Flags().StringVar(&contextText, "context", "", "Relevant preceding conversation, user decisions and scoped opt-outs, verbatim")
	handoff.Flags().StringVar(&file, "request-file", "", "Read original request bytes from a file")
	handoff.Flags().StringVar(&driver, "driver-id", "", "Original main driver; inferred inside Pi")
	handoff.Flags().StringVar(&key, "key", "", "Required durable idempotency key")
	handoff.Flags().StringVar(&lead, "lead-request", "", "Explicit lead request for a main-mediated sibling handoff or continuation")
	handoff.Flags().BoolVar(&queue, "queue", false, "Persist without starting a model session")
	command.AddCommand(handoff)
	var offset int
	inspect := &cobra.Command{Use: "inspect <subdriver-id>", Short: "Inspect a sub-driver owner, session, goals, pending events and worker links in pages of 20", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := get().store.SubdriverPage(args[0], offset)
		if err != nil {
			return err
		}
		return print(p)
	}}
	inspect.Flags().IntVar(&offset, "offset", 0, "Page offset; follow the returned next command")
	command.AddCommand(inspect)
	command.AddCommand(&cobra.Command{Use: "request <request-id>", Short: "Read exact original request and conversation context", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		r, err := get().store.SubdriverRequest(args[0])
		if err != nil {
			return err
		}
		if err = subdriverRequestScope(r); err != nil {
			return err
		}
		return print(r)
	}})
	command.AddCommand(&cobra.Command{Use: "event <event-id>", Short: "Read exact sub-driver event content and correlation", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		e, err := get().store.SubdriverEvent(id)
		if err != nil {
			return err
		}
		r, err := get().store.SubdriverRequest(e.RequestID)
		if err != nil {
			return err
		}
		if err = subdriverRequestScope(r); err != nil {
			return err
		}
		return print(e)
	}})
	command.AddCommand(&cobra.Command{Use: "notification <notification-id>", Short: "Read a complete worker notification for the current sub-driver", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		n, err := get().store.Notification(args[0])
		if err != nil {
			return err
		}
		f, err := control.SubdriverEnvironmentFence()
		if err != nil {
			return err
		}
		if f.ID != "" && n.TargetDriverID != "coordinator:"+f.ID {
			return fmt.Errorf("notification is outside sub-driver scope")
		}
		return print(n)
	}})
	var replyDriver, replyKey string
	var replyTo int64
	reply := &cobra.Command{Use: "reply <request-id> <text>", Short: "Correlate a user reply to the exact sub-driver question", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		owner, err := resolveDriverID(replyDriver, cmd.Flags().Changed("driver-id"), true)
		if err != nil {
			return err
		}
		e, err := get().store.SubdriverReply(args[0], owner, replyKey, args[1], replyTo)
		if err != nil {
			return err
		}
		return print(e)
	}}
	reply.Flags().StringVar(&replyDriver, "driver-id", "", "Original main driver")
	reply.Flags().StringVar(&replyKey, "key", "", "Required idempotency key")
	reply.Flags().Int64Var(&replyTo, "reply-to", 0, "Required sub-driver question event ID")
	command.AddCommand(reply)
	var returnKey, kind string
	ret := &cobra.Command{Use: "return <request-id> <text>", Short: "Persist a correlated return; result completes the request", Long: "Persist a correlated question, result, blocker, or handoff without lifecycle authority. A result immediately completes the conversational request, not a worker or artifact. Reserve results for actual completion, not assignment or progress. Dispatch, inspect, and confirm successful worker spawn before reporting assignment. Late corrections, questions, blockers, and handoffs do not reopen a completed request; route further work as a new correlated open intake with subdriver handoff --lead-request. Replies to nonterminal questions, blockers, or handoffs continue through subdriver reply.", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		f, err := control.SubdriverEnvironmentFence()
		if err != nil {
			return err
		}
		e, err := get().store.SubdriverReturn(f, args[0], returnKey, kind, args[1])
		if err != nil {
			return err
		}
		return print(e)
	}}
	ret.Flags().StringVar(&returnKey, "key", "", "Required durable return key")
	ret.Flags().StringVar(&kind, "kind", "", "question, result (completes request), blocker, or handoff")
	command.AddCommand(ret)
	command.AddCommand(&cobra.Command{Use: "handled <event-id>", Short: "Record substantive intake or reply handling, not approval", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		f, err := control.SubdriverEnvironmentFence()
		if err != nil {
			return err
		}
		if err = get().store.HandleSubdriverEvent(f, id); err != nil {
			return err
		}
		return print(map[string]any{"handled": id})
	}})
	var dispatchKey, feature, title, acceptance, deliverable string
	dispatch := &cobra.Command{Use: "dispatch <request-id> <objective>", Short: "Idempotently queue one ordinary worker under this durable owner", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		f, err := control.SubdriverEnvironmentFence()
		if err != nil {
			return err
		}
		task, err := get().store.DispatchSubdriverWorker(f, args[0], dispatchKey, model.Task{FeatureKey: feature, Title: title, Objective: args[1], AcceptanceCriteria: acceptance, Deliverable: deliverable})
		if err != nil {
			return err
		}
		return print(task)
	}}
	dispatch.Flags().StringVar(&dispatchKey, "key", "", "Required stable plan step key")
	dispatch.Flags().StringVar(&feature, "feature", "", "Worker feature key")
	dispatch.Flags().StringVar(&title, "title", "", "Optional worker title")
	dispatch.Flags().StringVar(&acceptance, "acceptance", "", "Worker acceptance criteria")
	dispatch.Flags().StringVar(&deliverable, "deliverable", "code", "code or report")
	command.AddCommand(dispatch)
	var foreground bool
	var resumeModel string
	var resumeGeneration int
	resume := &cobra.Command{Use: "resume <subdriver-id>", Short: "Run a fresh bounded session only when durable work is pending", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var generation *int
		if cmd.Flags().Changed("generation") {
			generation = &resumeGeneration
		}
		c, err := get().control.ResumeSubdriverWithModelSelection(args[0], foreground, resumeModel, cmd.Flags().Changed("model"), generation)
		if err != nil {
			return err
		}
		return print(c)
	}}
	resume.Flags().BoolVar(&foreground, "foreground", false, "Run synchronously in configured headless runtime")
	resume.Flags().StringVar(&resumeModel, "model", "", "Override the retained harness-specific model; requires --generation; empty selects the harness-native default")
	resume.Flags().IntVar(&resumeGeneration, "generation", 0, "Exact inspected generation; required with --model")
	command.AddCommand(resume)
	command.AddCommand(&cobra.Command{Use: "context <subdriver-id>", Short: "Preview bounded fresh-session context without starting a model", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		text, err := get().control.SubdriverContext(args[0])
		if err != nil {
			return err
		}
		return print(map[string]string{"context": text})
	}})
	var recoverGeneration int
	var launchAbsentReason string
	recover := &cobra.Command{Use: "recover <subdriver-id>", Short: "Release a held session only after recorded processes and endpoint are proven absent", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := get().control.RecoverSubdriver(args[0], recoverGeneration, launchAbsentReason); err != nil {
			return err
		}
		c, err := get().store.Subdriver(args[0])
		if err != nil {
			return err
		}
		return print(c)
	}}
	recover.Flags().IntVar(&recoverGeneration, "generation", 0, "Exact inspected generation")
	recover.Flags().StringVar(&launchAbsentReason, "launch-absent", "", "Explicit operator confirmation of absent unrecorded launch effects; never overrides a live PID or endpoint")
	command.AddCommand(recover)
	var from, to string
	adopt := &cobra.Command{Use: "adopt-request <request-id>", Short: "Explicitly move only the main return route, never worker ownership", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		owner, err := resolveDriverID(to, cmd.Flags().Changed("driver-id"), true)
		if err != nil {
			return err
		}
		if err = get().store.AdoptSubdriverRequest(args[0], from, owner); err != nil {
			return err
		}
		r, err := get().store.SubdriverRequest(args[0])
		if err != nil {
			return err
		}
		return print(r)
	}}
	adopt.Flags().StringVar(&from, "from-driver", "", "Previous main return owner")
	adopt.Flags().StringVar(&to, "driver-id", "", "Replacement main return owner")
	command.AddCommand(adopt)
	var generation int
	var token string
	run := &cobra.Command{Use: "_run <subdriver-id>", Hidden: true, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return get().control.RunSubdriver(model.SubdriverFence{ID: args[0], Generation: generation, Token: token}, cmd.OutOrStdout())
	}}
	run.Flags().IntVar(&generation, "generation", 0, "")
	run.Flags().StringVar(&token, "token", "", "")
	command.AddCommand(run)
	return command
}
func subdriverRequestScope(r model.SubdriverRequest) error {
	f, err := control.SubdriverEnvironmentFence()
	if err != nil {
		return err
	}
	if f.ID != "" && r.SubdriverID != f.ID {
		return fmt.Errorf("request is outside sub-driver scope")
	}
	return nil
}

func subdriverCommandFence(app *application, cmd *cobra.Command, args []string) error {
	path := strings.TrimPrefix(cmd.CommandPath(), "shephrd ")
	f, err := control.SubdriverEnvironmentFence()
	if err != nil {
		return err
	}
	if os.Getenv("SHEPHRD_WORKER") == "1" && (strings.HasPrefix(path, "subdriver ") || path == "wake pump") {
		return fmt.Errorf("ordinary workers cannot coordinate or create supervisors")
	}
	if f.ID == "" {
		if flag := cmd.Flags().Lookup("driver-id"); flag != nil && model.IsSubdriverOwner(flag.Value.String()) {
			return fmt.Errorf("sub-driver owner identity requires a current fenced session")
		}
		if len(args) > 0 && (strings.HasPrefix(path, "worker ") || strings.HasPrefix(path, "task ")) && path != "task inspect" && path != "task annotations" {
			task, err := app.store.Task(args[0])
			if err == nil && model.IsSubdriverOwner(task.DriverID) {
				return fmt.Errorf("worker is supervised by %s; reply to its sub-driver request instead", task.DriverID)
			}
		}
		return nil
	}
	if path == "subdriver _run" {
		generation, _ := cmd.Flags().GetInt("generation")
		token, _ := cmd.Flags().GetString("token")
		if len(args) != 1 || args[0] != f.ID || generation != f.Generation || token != f.Token {
			return fmt.Errorf("runner arguments conflict with sub-driver environment identity")
		}
		return nil
	}
	if err := app.store.CheckSubdriverFence(f); err != nil {
		return err
	}
	switch path {
	case "subdriver request", "subdriver return", "subdriver dispatch", "subdriver handled", "subdriver event", "subdriver notification":
		return nil
	case "subdriver inspect", "subdriver context":
		if len(args) == 1 && args[0] == f.ID {
			return nil
		}
	case "task obligations":
		flag := cmd.Flags().Lookup("driver-id")
		if flag != nil && flag.Value.String() != "" && flag.Value.String() != "coordinator:"+f.ID {
			return fmt.Errorf("obligations owner is outside sub-driver scope")
		}
		if all := cmd.Flags().Lookup("all-drivers"); all != nil && all.Value.String() == "true" {
			return fmt.Errorf("cross-scope obligations are not permitted")
		}
		return nil
	case "wake ack", "wake renew":
		return nil
	case "task inspect", "task annotate", "task annotations", "task attest-delivery", "task attest-report-recovery", "task verify-delivery", "task archive", "worker spawn", "worker send", "worker retry", "worker relaunch", "worker stop", "worker status", "worker peek", "worker focus", "workspace release":
		if discard := cmd.Flags().Lookup("discard"); discard != nil && discard.Value.String() == "true" {
			return fmt.Errorf("discard authorization remains with main; return a correlated question")
		}
		if len(args) > 0 {
			return app.store.SubdriverTaskFence(f, args[0])
		}
	}
	return fmt.Errorf("%s is not permitted in a sub-driver session; route missing scope or authority to main", path)
}
