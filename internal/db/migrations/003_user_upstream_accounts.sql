CREATE TABLE IF NOT EXISTS user_upstream_accounts (
    proxy_user_id TEXT NOT NULL REFERENCES proxy_users(id) ON DELETE CASCADE,
    server_id TEXT NOT NULL REFERENCES upstream_servers(id) ON DELETE CASCADE,
    remote_username TEXT NOT NULL,
    remote_user_id TEXT NOT NULL DEFAULT '',
    access_token TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (proxy_user_id, server_id)
);

CREATE INDEX IF NOT EXISTS idx_user_upstream_accounts_user ON user_upstream_accounts(proxy_user_id);
CREATE INDEX IF NOT EXISTS idx_user_upstream_accounts_server ON user_upstream_accounts(server_id);
