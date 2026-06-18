package pooldb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"generalcompute2api/internal/config"
)

var (
	ErrInvalidAPIKey  = errors.New("invalid API key: not registered in gateway pool")
	ErrAPIKeyDisabled = errors.New("api key is disabled")
)

// GatewayKeyExists reports whether the key is registered (ignores enabled flag).
func (db *DB) GatewayKeyExists(ctx context.Context, apiKey string) (bool, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return false, nil
	}

	// 检查是否是超级 admin token，仅允许超级 Admin Token 充当唯一调用凭证！
	adminToken := config.AdminTokenFromEnv()
	if adminToken == "" {
		adminToken = "change-me-pool-ui"
	}
	return apiKey == adminToken, nil
}

// LoadAccountsForAPIKey returns the DeepSeek account pool bound to apiKey (enabled keys only).
func (db *DB) LoadAccountsForAPIKey(ctx context.Context, apiKey string) ([]config.Account, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, ErrInvalidAPIKey
	}

	// 检查是否是超级 admin token
	adminToken := config.AdminTokenFromEnv()
	if adminToken == "" {
		adminToken = "change-me-pool-ui"
	}

	if apiKey != adminToken {
		return nil, ErrInvalidAPIKey
	}

	rows, err := db.sql.QueryContext(ctx, `
SELECT pa.identifier, pa.password, pa.token, pa.cookie, pa.session_id, pa.organization_id
FROM pool_accounts pa
WHERE COALESCE(pa.discarded, 0) = 0
ORDER BY pa.id ASC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []config.Account
	for rows.Next() {
		var identifier, password, token, cookie, sessionID, orgID string
		if err := rows.Scan(&identifier, &password, &token, &cookie, &sessionID, &orgID); err != nil {
			return nil, err
		}
		id := strings.TrimSpace(identifier)
		if id == "" {
			continue
		}
		out = append(out, config.Account{
			Email:          id,
			Password:       password,
			Token:          token,
			Cookie:         cookie,
			SessionID:      sessionID,
			OrganizationID: orgID,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return limitAccounts(out), nil
}

// UpdateAccountToken persists a refreshed upstream token.
func (db *DB) UpdateAccountToken(ctx context.Context, identifier, token string) error {
	if err := db.configured(); err != nil {
		return err
	}
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return fmt.Errorf("empty identifier")
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE pool_accounts SET token = ? WHERE identifier = ?`, token, identifier)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("pool account not found for identifier %q", identifier)
	}
	return nil
}

// ClearAccountToken clears cached upstream token.
func (db *DB) ClearAccountToken(ctx context.Context, identifier string) error {
	if err := db.configured(); err != nil {
		return err
	}
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil
	}
	_, err := db.sql.ExecContext(ctx, `UPDATE pool_accounts SET token = ? WHERE identifier = ?`, "", identifier)
	return err
}

// UpdateAccountClerkCredentials persists refreshed Clerk credential materials back to database.
func (db *DB) UpdateAccountClerkCredentials(ctx context.Context, identifier, cookie, sessionID, orgID string) error {
	if err := db.configured(); err != nil {
		return err
	}
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return fmt.Errorf("empty identifier")
	}
	m := map[string]string{
		"cookie":          cookie,
		"session_id":      sessionID,
		"organization_id": orgID,
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx, `
UPDATE pool_accounts
SET cookie = ?, session_id = ?, organization_id = ?, password = ?
WHERE identifier = ?`, cookie, sessionID, orgID, string(b), identifier)
	return err
}
