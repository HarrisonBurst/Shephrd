package cli

import (
	"strings"

	"shephrd/internal/config"
	"shephrd/internal/fault"
)

type Origin struct {
	Local bool
	As    string
}

type Caller struct {
	Kind     string `json:"kind"`
	Name     string `json:"name,omitempty"`
	Operator bool   `json:"operator"`
}

func (c Caller) String() string {
	if c.Name == "" {
		return c.Kind
	}
	return c.Kind + ":" + c.Name
}

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
			return Caller{}, fault.New("invalid_token", "the run token does not belong to a current run")
		}
		return Caller{}, fault.New("invalid_token", "the call token does not belong to a current plugin call")
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
