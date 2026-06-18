package pooldb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"generalcompute2api/internal/config"
)

// PoolAccountCredential is login material for a bound pool account.
type PoolAccountCredential struct {
	Identifier string
	Password   string
	Token      string
	Discarded  bool
}

// GetPoolAccountCredential loads one account bound to apiKey.
func (db *DB) GetPoolAccountCredential(ctx context.Context, apiKey, identifier string) (PoolAccountCredential, error) {
	if err := db.configured(); err != nil {
		return PoolAccountCredential{}, err
	}
	apiKey = strings.TrimSpace(apiKey)
	identifier = strings.TrimSpace(identifier)
	var cred PoolAccountCredential
	var discarded int
	var password, cookie, sessionID, orgID string
	err := db.sql.QueryRowContext(ctx, `
SELECT pa.identifier, pa.password, COALESCE(pa.token, ''), COALESCE(pa.cookie, ''), COALESCE(pa.session_id, ''), COALESCE(pa.organization_id, ''), COALESCE(pa.discarded, 0)
FROM pool_bindings pb
INNER JOIN pool_accounts pa ON pa.id = pb.account_id
WHERE pb.api_key = ? AND pa.identifier = ?
`, apiKey, identifier).Scan(&cred.Identifier, &password, &cred.Token, &cookie, &sessionID, &orgID, &discarded)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PoolAccountCredential{}, fmt.Errorf("account not in pool")
		}
		return PoolAccountCredential{}, err
	}
	cred.Discarded = discarded != 0
	
	// 动态合成凭证以兼容测号接口
	if cookie != "" || sessionID != "" || orgID != "" {
		m := map[string]string{
			"cookie":          cookie,
			"session_id":      sessionID,
			"organization_id": orgID,
		}
		if b, err := json.Marshal(m); err == nil {
			cred.Password = string(b)
		} else {
			cred.Password = password
		}
	} else {
		cred.Password = password
	}
	return cred, nil
}

// ListPoolAccountCredentials returns credentials for bound accounts (optionally filtered).
func (db *DB) ListPoolAccountCredentials(ctx context.Context, apiKey string, identifiers []string, activeOnly bool) ([]PoolAccountCredential, error) {
	if err := db.configured(); err != nil {
		return nil, err
	}
	apiKey = strings.TrimSpace(apiKey)
	q := `
SELECT pa.identifier, pa.password, COALESCE(pa.token, ''), COALESCE(pa.cookie, ''), COALESCE(pa.session_id, ''), COALESCE(pa.organization_id, ''), COALESCE(pa.discarded, 0)
FROM pool_bindings pb
INNER JOIN pool_accounts pa ON pa.id = pb.account_id
WHERE pb.api_key = ?`
	if activeOnly {
		q += ` AND COALESCE(pa.discarded, 0) = 0`
	}
	q += ` ORDER BY pb.position ASC, pa.id ASC`
	rows, err := db.sql.QueryContext(ctx, q, apiKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	want := map[string]struct{}{}
	for _, id := range identifiers {
		id = strings.TrimSpace(id)
		if id != "" {
			want[id] = struct{}{}
		}
	}
	filter := len(want) > 0
	var out []PoolAccountCredential
	for rows.Next() {
		var cred PoolAccountCredential
		var discarded int
		var password, cookie, sessionID, orgID string
		if err := rows.Scan(&cred.Identifier, &password, &cred.Token, &cookie, &sessionID, &orgID, &discarded); err != nil {
			return nil, err
		}
		cred.Discarded = discarded != 0
		
		// 动态合成凭证以兼容测号接口
		if cookie != "" || sessionID != "" || orgID != "" {
			m := map[string]string{
				"cookie":          cookie,
				"session_id":      sessionID,
				"organization_id": orgID,
			}
			if b, err := json.Marshal(m); err == nil {
				cred.Password = string(b)
			} else {
				cred.Password = password
			}
		} else {
			cred.Password = password
		}
		
		if filter {
			if _, ok := want[cred.Identifier]; !ok {
				continue
			}
		}
		out = append(out, cred)
	}
	return out, rows.Err()
}

// AccountToConfig maps stored identifier + password to config.Account for DeepSeek login.
func AccountToConfig(identifier, password string) config.Account {
	identifier = strings.TrimSpace(identifier)
	acc := config.Account{Password: strings.TrimSpace(password)}
	if strings.Contains(identifier, "@") {
		acc.Email = identifier
	} else {
		acc.Mobile = identifier
	}
	return acc
}
