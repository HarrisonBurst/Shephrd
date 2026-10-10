package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/store"
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

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *app) callTokenCaller() (Caller, error) {
	db, err := a.store()
	if err != nil {
		return Caller{}, err
	}
	var name, actsAs, expires string
	err = db.QueryRowContext(a.ctx, `SELECT plugin, acts_as, expires_at FROM call_tokens WHERE hash = ?`, tokenHash(a.req.CallToken)).Scan(&name, &actsAs, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && expires < store.Timestamp(time.Now())) {
		return Caller{}, fault.New("invalid_token", "the call token does not belong to a current plugin call")
	}
	if err != nil {
		return Caller{}, err
	}
	if actsAs == "" {
		return Caller{Kind: "plugin", Name: name}, nil
	}
	var caller Caller
	if err := json.Unmarshal([]byte(actsAs), &caller); err != nil {
		return Caller{}, err
	}
	return caller, nil
}

func (a *app) issueToken(pluginName string, actsAs *Caller, ttl time.Duration) (string, func(), error) {
	db, err := a.store()
	if err != nil {
		return "", nil, err
	}
	encoded := ""
	if actsAs != nil {
		body, err := json.Marshal(actsAs)
		if err != nil {
			return "", nil, err
		}
		encoded = string(body)
	}
	token := newToken()
	hash := tokenHash(token)
	err = db.Write(a.ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`DELETE FROM call_tokens WHERE expires_at < ?`, store.Timestamp(tx.Now)); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO call_tokens (hash, plugin, acts_as, expires_at) VALUES (?, ?, ?, ?)`,
			hash, pluginName, encoded, store.Timestamp(tx.Now.Add(ttl)))
		return err
	})
	if err != nil {
		return "", nil, err
	}
	return token, func() { db.ExecContext(a.ctx, `DELETE FROM call_tokens WHERE hash = ?`, hash) }, nil
}
