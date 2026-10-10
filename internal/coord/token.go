package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"shephrd/internal/fault"
	"shephrd/internal/store"
)

// IssueCallToken lets one plugin call act back through ordinary commands:
// as the plugin itself, or with the permissions of the caller it serves.
func IssueCallToken(ctx context.Context, db *store.Store, plugin string, actsAs *Caller, ttl time.Duration) (string, func(), error) {
	encoded := ""
	if actsAs != nil {
		body, err := json.Marshal(actsAs)
		if err != nil {
			return "", nil, err
		}
		encoded = string(body)
	}
	token := newToken()
	hash := TokenHash(token)
	err := db.Write(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`DELETE FROM call_tokens WHERE expires_at < ?`, store.Timestamp(tx.Now)); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO call_tokens (hash, plugin, acts_as, expires_at) VALUES (?, ?, ?, ?)`,
			hash, plugin, encoded, store.Timestamp(tx.Now.Add(ttl)))
		return err
	})
	if err != nil {
		return "", nil, err
	}
	return token, func() { db.ExecContext(context.Background(), `DELETE FROM call_tokens WHERE hash = ?`, hash) }, nil
}

func CallCaller(q Querier, token string) (Caller, error) {
	var name, actsAs, expires string
	err := q.QueryRow(`SELECT plugin, acts_as, expires_at FROM call_tokens WHERE hash = ?`, TokenHash(token)).Scan(&name, &actsAs, &expires)
	if errors.Is(err, sql.ErrNoRows) || err == nil && expires < store.Timestamp(time.Now()) {
		return Caller{}, fault.New("invalid_token", "the call token does not belong to a current plugin call")
	}
	if err != nil {
		return Caller{}, err
	}
	if actsAs == "" {
		return Caller{Kind: "plugin", Name: name}, nil
	}
	var caller Caller
	return caller, json.Unmarshal([]byte(actsAs), &caller)
}
