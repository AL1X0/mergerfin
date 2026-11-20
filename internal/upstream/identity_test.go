package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AL1X0/mergerfin/internal/db"
	"github.com/AL1X0/mergerfin/internal/idgen"
)

func TestBuildMediaBrowserHeader(t *testing.T) {
	t.Run("default_background_header", func(t *testing.T) {
		got := BuildMediaBrowserHeader(SessionIdentity{}, "tok-123")
		if !strings.Contains(got, `Client="MergerFin"`) || !strings.Contains(got, `Device="MergerFin Proxy"`) || !strings.Contains(got, `Token="tok-123"`) {
			t.Fatalf("unexpected default header: %s", got)
		}
	})

	t.Run("unmapped_user_tags_device_with_username", func(t *testing.T) {
		got := BuildMediaBrowserHeader(SessionIdentity{
			ProxyUsername: "test",
			ClientName:    "Swiftfin",
			DeviceName:    "iPhone",
			DeviceID:      "ABC-123",
			Version:       "1.2.3",
			IsMappedUser:  false,
		}, "shared-tok")

		for _, want := range []string{
			`Client="Swiftfin"`,
			`Device="test (iPhone)"`,
			`DeviceId="mf-test-abc-123"`,
			`Version="1.2.3"`,
			`Token="shared-tok"`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("header %q missing %q", got, want)
			}
		}
	})

	t.Run("mapped_user_uses_clean_device_name", func(t *testing.T) {
		got := BuildMediaBrowserHeader(SessionIdentity{
			ProxyUsername: "test",
			ClientName:    "Swiftfin",
			DeviceName:    "iPhone",
			DeviceID:      "ABC-123",
			Version:       "1.2.3",
			IsMappedUser:  true,
		}, "user-tok")

		if !strings.Contains(got, `Device="iPhone"`) {
			t.Errorf("expected clean Device=\"iPhone\", got %q", got)
		}
		if !strings.Contains(got, `Token="user-tok"`) {
			t.Errorf("expected user token in header, got %q", got)
		}
	})

	t.Run("sanitizes_quotes_and_newlines", func(t *testing.T) {
		got := BuildMediaBrowserHeader(SessionIdentity{
			ProxyUsername: "al\"ice\r\n",
			ClientName:    "Web\"Client",
			DeviceName:    "Chrome\n",
			DeviceID:      "dev/123",
		}, "tok")
		if strings.ContainsAny(got, "\r\n") {
			t.Fatalf("header must not contain CR/LF: %q", got)
		}
		if !strings.Contains(got, `Device="alice (Chrome)"`) {
			t.Fatalf("unexpected sanitized device: %q", got)
		}
	})
}

func TestPoolPerUserUpstreamAccountMapping(t *testing.T) {
	ctx := context.Background()

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ServerName": "MockFin",
				"Version":    "12.4.0",
			})
		case "/Users/AuthenticateByName":
			var req struct {
				Username string `json:"Username"`
				Pw       string `json:"Pw"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			switch req.Username {
			case "service_acct":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"User":        map[string]any{"Id": "11111111111111111111111111111111"},
					"AccessToken": "service-secret-token",
				})
			case "alice_remote":
				if req.Pw != "alice-pass" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"User":        map[string]any{"Id": "22222222222222222222222222222222"},
					"AccessToken": "alice-secret-token",
				})
			default:
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockUpstream.Close()

	database, err := db.Open(ctx, ":memory:", "TestMergerFin")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	aliceID := idgen.DeterministicHex32("user:alice")
	bobID := idgen.DeterministicHex32("user:bob")
	if err := database.CreateUser(ctx, db.ProxyUser{ID: aliceID, Username: "alice", PasswordHash: "hash"}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if err := database.CreateUser(ctx, db.ProxyUser{ID: bobID, Username: "bob", PasswordHash: "hash"}); err != nil {
		t.Fatalf("create bob: %v", err)
	}

	pool, err := NewPool(database, "super-secret-key", nil)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}

	srv, err := pool.AddOrUpdateServer(ctx, "", "MockFin", mockUpstream.URL, "service_acct", "svc-pass", 10, "proxy", true)
	if err != nil {
		t.Fatalf("add server: %v", err)
	}

	// Link alice -> alice_remote
	acc, err := pool.SetUserUpstreamAccount(ctx, aliceID, srv.ID, "alice_remote", "alice-pass")
	if err != nil {
		t.Fatalf("set user upstream account: %v", err)
	}
	if !strings.HasPrefix(acc.AccessToken, encPrefix) {
		t.Fatalf("expected encrypted token at rest, got %q", acc.AccessToken)
	}

	// Alice gets her mapped upstream credentials
	aliceSrv, isMapped, err := pool.GetDecryptedServerForUser(ctx, srv.ID, aliceID)
	if err != nil {
		t.Fatalf("get server for alice: %v", err)
	}
	if !isMapped {
		t.Fatalf("expected alice to be mapped")
	}
	if aliceSrv.Username != "alice_remote" || aliceSrv.RemoteUserID != "22222222222222222222222222222222" || aliceSrv.AccessToken != "alice-secret-token" {
		t.Fatalf("unexpected alice server creds: %+v", aliceSrv)
	}

	// Bob falls back to the default service account
	bobSrv, isMapped, err := pool.GetDecryptedServerForUser(ctx, srv.ID, bobID)
	if err != nil {
		t.Fatalf("get server for bob: %v", err)
	}
	if isMapped {
		t.Fatalf("expected bob NOT to be mapped")
	}
	if bobSrv.Username != "service_acct" || bobSrv.AccessToken != "service-secret-token" {
		t.Fatalf("unexpected bob server creds: %+v", bobSrv)
	}

	// ListEnabledDecryptedServersForUser returns mapped map properly
	listAlice, mappedMap, err := pool.ListEnabledDecryptedServersForUser(ctx, aliceID)
	if err != nil {
		t.Fatalf("list servers for alice: %v", err)
	}
	if len(listAlice) != 1 || !mappedMap[srv.ID] || listAlice[0].AccessToken != "alice-secret-token" {
		t.Fatalf("unexpected list for alice: %+v, mapped=%v", listAlice, mappedMap)
	}

	// Unlinking reverts alice to service account
	if err := pool.DeleteUserUpstreamAccount(ctx, aliceID, srv.ID); err != nil {
		t.Fatalf("delete user upstream account: %v", err)
	}
	aliceAfter, isMapped, err := pool.GetDecryptedServerForUser(ctx, srv.ID, aliceID)
	if err != nil {
		t.Fatalf("get server for alice after unlink: %v", err)
	}
	if isMapped || aliceAfter.AccessToken != "service-secret-token" {
		t.Fatalf("expected fallback after unlink, got mapped=%v token=%q", isMapped, aliceAfter.AccessToken)
	}
}
