package cli

import (
	"strings"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
)

type Origin struct {
	Local bool
	As    string
}

type Caller = coord.Caller

func (a *app) identity() (Caller, error) {
	if a.caller != nil {
		return *a.caller, nil
	}
	caller, err := a.resolveCaller()
	if err != nil {
		return Caller{}, err
	}
	a.caller = &caller
	return caller, nil
}

func (a *app) resolveCaller() (Caller, error) {
	if a.req.RunToken != "" || a.req.CallToken != "" {
		if a.origin.As != "" {
			return Caller{}, fault.New("as_not_allowed", "--as cannot be used by a run or a plugin")
		}
		if a.req.RunToken != "" && a.req.CallToken != "" {
			return Caller{}, fault.New("invalid_token", "a request carries either a run token or a call token, not both")
		}
		if a.req.RunToken != "" {
			db, err := a.store()
			if err != nil {
				return Caller{}, err
			}
			caller, run, err := coord.RunCaller(db, a.req.RunToken)
			if err != nil {
				return Caller{}, err
			}
			if !strings.HasPrefix(a.req.Argv[0], "_") {
				a.turnBudget(caller, run)
			}
			return caller, nil
		}
		return a.callTokenCaller()
	}
	if a.origin.As != "" {
		kind, name, _ := strings.Cut(a.origin.As, ":")
		if kind != "driver" || !config.NamePattern.MatchString(name) {
			return Caller{}, fault.New("usage", "--as takes driver:<name>, got %q", a.origin.As)
		}
		return Caller{Kind: "driver", Name: name, Operator: a.origin.Local}, nil
	}
	if !a.origin.Local {
		return Caller{}, fault.New("unidentified", "the request carries no identity")
	}
	cfg, err := a.config()
	if err != nil {
		return Caller{}, err
	}
	if cfg.Driver != "" {
		return Caller{Kind: "driver", Name: cfg.Driver, Operator: true}, nil
	}
	return Caller{Kind: "operator", Operator: true}, nil
}

func (a *app) requireOperator() error {
	caller, err := a.identity()
	if err != nil {
		return err
	}
	if !caller.Operator {
		return fault.New("operator_only", "only the operator on the home host can do this")
	}
	return nil
}

func (a *app) callTokenCaller() (Caller, error) {
	db, err := a.store()
	if err != nil {
		return Caller{}, err
	}
	return coord.CallCaller(db, a.req.CallToken)
}

func (a *app) issueToken(pluginName string, actsAs *Caller, ttl time.Duration) (string, func(), error) {
	db, err := a.store()
	if err != nil {
		return "", nil, err
	}
	return coord.IssueCallToken(a.ctx, db, pluginName, actsAs, ttl)
}
