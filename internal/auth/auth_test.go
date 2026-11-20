package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AL1X0/mergerfin/internal/db"
	"github.com/AL1X0/mergerfin/internal/idgen"
)

func TestTouchSessionThrottlingAndImmediateRevocation(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:", "AuthTest")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	user, err := EnsureAdminUser(ctx, database, "admin", "secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser: %v", err)
	}

	token := idgen.NewRandomHex32()
	t.Cleanup(func() { sessionTouchCache.Delete(token) })

	if err := database.UpsertSession(ctx, db.ProxySession{
		Token:      token,
		UserID:     user.ID,
		DeviceID:   "dev-1",
		ClientName: "Web",
		DeviceName: "Browser",
		AppVersion: "12.4.0",
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	const sentinelTS = "2020-01-01 00:00:00"
	setOldLastSeen := func() {
		t.Helper()
		if _, err := database.SqlDB().ExecContext(ctx, `UPDATE proxy_sessions SET last_seen_at = ? WHERE token = ?`, sentinelTS, token); err != nil {
			t.Fatalf("update last_seen_at: %v", err)
		}
	}
	getLastSeen := func() string {
		t.Helper()
		var ts string
		if err := database.SqlDB().QueryRowContext(ctx, `SELECT CAST(last_seen_at AS TEXT) FROM proxy_sessions WHERE token = ?`, token).Scan(&ts); err != nil {
			t.Fatalf("query last_seen_at: %v", err)
		}
		return ts
	}

	handler := Authenticate(database)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	doReq := func() int {
		req := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
		req.Header.Set("X-Emby-Token", token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// 1. First request touches session.
	setOldLastSeen()
	if code := doReq(); code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	if ts := getLastSeen(); ts == sentinelTS {
		t.Fatalf("expected TouchSession to update last_seen_at on first request, still %q", ts)
	}

	// 2. Subsequent request within 5 minutes skips TouchSession write.
	setOldLastSeen()
	if code := doReq(); code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}
	if ts := getLastSeen(); ts != sentinelTS {
		t.Fatalf("expected TouchSession to be throttled within %s, but last_seen_at changed to %q", touchThrottleInterval, ts)
	}

	// 3. Once throttle window elapses, TouchSession runs again.
	sessionTouchCache.Store(token, time.Now().Add(-touchThrottleInterval-time.Second))
	if code := doReq(); code != http.StatusOK {
		t.Fatalf("third request status = %d, want 200", code)
	}
	if ts := getLastSeen(); ts == sentinelTS {
		t.Fatalf("expected TouchSession to update last_seen_at after interval elapsed, still %q", ts)
	}

	// 4. Logout / session deletion takes effect immediately despite touch cache.
	if err := database.DeleteSession(ctx, token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if code := doReq(); code != http.StatusUnauthorized {
		t.Fatalf("request after DeleteSession status = %d, want 401", code)
	}
}
