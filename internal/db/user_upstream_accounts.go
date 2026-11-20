package db

import (
	"context"
	"fmt"
	"time"
)

// UserUpstreamAccount maps a local MergerFin proxy user to a dedicated account
// on a specific upstream Jellyfin server.
type UserUpstreamAccount struct {
	ProxyUserID    string
	ServerID       string
	RemoteUsername string
	RemoteUserID   string
	AccessToken    string
	UpdatedAt      time.Time
}

// UpsertUserUpstreamAccount inserts or updates a per-user upstream account mapping.
func (d *DB) UpsertUserUpstreamAccount(ctx context.Context, acc UserUpstreamAccount) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		INSERT INTO user_upstream_accounts (proxy_user_id, server_id, remote_username, remote_user_id, access_token, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(proxy_user_id, server_id) DO UPDATE SET
			remote_username = excluded.remote_username,
			remote_user_id = excluded.remote_user_id,
			access_token = excluded.access_token,
			updated_at = CURRENT_TIMESTAMP
	`, acc.ProxyUserID, acc.ServerID, acc.RemoteUsername, acc.RemoteUserID, acc.AccessToken)
	if err != nil {
		return fmt.Errorf("upsert user upstream account: %w", err)
	}
	return nil
}

// GetUserUpstreamAccount returns the mapped upstream account for (proxyUserID, serverID),
// or sql.ErrNoRows when no mapping exists.
func (d *DB) GetUserUpstreamAccount(ctx context.Context, proxyUserID, serverID string) (*UserUpstreamAccount, error) {
	var acc UserUpstreamAccount
	err := d.reader().QueryRowContext(ctx, `
		SELECT proxy_user_id, server_id, remote_username, remote_user_id, access_token, updated_at
		FROM user_upstream_accounts
		WHERE proxy_user_id = ? AND server_id = ?
	`, proxyUserID, serverID).Scan(
		&acc.ProxyUserID,
		&acc.ServerID,
		&acc.RemoteUsername,
		&acc.RemoteUserID,
		&acc.AccessToken,
		&acc.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &acc, nil
}

// ListUserUpstreamAccountsByUser returns all upstream account mappings for a local proxy user.
func (d *DB) ListUserUpstreamAccountsByUser(ctx context.Context, proxyUserID string) ([]UserUpstreamAccount, error) {
	rows, err := d.reader().QueryContext(ctx, `
		SELECT proxy_user_id, server_id, remote_username, remote_user_id, access_token, updated_at
		FROM user_upstream_accounts
		WHERE proxy_user_id = ?
		ORDER BY server_id ASC
	`, proxyUserID)
	if err != nil {
		return nil, fmt.Errorf("list user upstream accounts: %w", err)
	}
	defer rows.Close()

	var out []UserUpstreamAccount
	for rows.Next() {
		var acc UserUpstreamAccount
		if err := rows.Scan(
			&acc.ProxyUserID,
			&acc.ServerID,
			&acc.RemoteUsername,
			&acc.RemoteUserID,
			&acc.AccessToken,
			&acc.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan user upstream account: %w", err)
		}
		out = append(out, acc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user upstream accounts: %w", err)
	}
	return out, nil
}

// ListAllUserUpstreamAccounts returns every per-user upstream account mapping.
func (d *DB) ListAllUserUpstreamAccounts(ctx context.Context) ([]UserUpstreamAccount, error) {
	rows, err := d.reader().QueryContext(ctx, `
		SELECT proxy_user_id, server_id, remote_username, remote_user_id, access_token, updated_at
		FROM user_upstream_accounts
		ORDER BY proxy_user_id ASC, server_id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list all user upstream accounts: %w", err)
	}
	defer rows.Close()

	var out []UserUpstreamAccount
	for rows.Next() {
		var acc UserUpstreamAccount
		if err := rows.Scan(
			&acc.ProxyUserID,
			&acc.ServerID,
			&acc.RemoteUsername,
			&acc.RemoteUserID,
			&acc.AccessToken,
			&acc.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan user upstream account: %w", err)
		}
		out = append(out, acc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate all user upstream accounts: %w", err)
	}
	return out, nil
}

// DeleteUserUpstreamAccount removes the upstream account mapping for (proxyUserID, serverID).
func (d *DB) DeleteUserUpstreamAccount(ctx context.Context, proxyUserID, serverID string) error {
	_, err := d.sqlDB.ExecContext(ctx, `
		DELETE FROM user_upstream_accounts
		WHERE proxy_user_id = ? AND server_id = ?
	`, proxyUserID, serverID)
	if err != nil {
		return fmt.Errorf("delete user upstream account: %w", err)
	}
	return nil
}
