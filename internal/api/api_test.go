package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/AL1X0/mergerfin/internal/admin"
	"github.com/AL1X0/mergerfin/internal/api"
	"github.com/AL1X0/mergerfin/internal/auth"
	"github.com/AL1X0/mergerfin/internal/config"
	"github.com/AL1X0/mergerfin/internal/db"
	"github.com/AL1X0/mergerfin/internal/idgen"
	"github.com/AL1X0/mergerfin/internal/upstream"
)

type testEnv struct {
	db     *db.DB
	wsHub  *api.WSHub
	router http.Handler
	admin  *db.ProxyUser
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()

	database, err := db.Open(ctx, ":memory:", "MergerFin Test")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	adminUser, err := auth.EnsureAdminUser(ctx, database, "admin", "secretpass")
	if err != nil {
		t.Fatalf("auth.EnsureAdminUser: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := upstream.NewPool(database, "test-secret-key", logger)
	if err != nil {
		t.Fatalf("upstream.NewPool: %v", err)
	}

	wsHub := api.NewWSHub(database, logger)
	t.Cleanup(wsHub.CloseAll)

	adminHandler := admin.NewHandler(database, pool, nil, logger)
	cfg := &config.Config{
		Port:       "8096",
		ServerName: "MergerFin Test",
		AdminUser:  "admin",
		AdminPass:  "secretpass",
	}

	router := api.NewRouter(api.RouterDeps{
		Config: cfg,
		DB:     database,
		Pool:   pool,
		Admin:  adminHandler,
		WSHub:  wsHub,
		Syncer: nil,
		Logger: logger,
	})

	return &testEnv{
		db:     database,
		wsHub:  wsHub,
		router: router,
		admin:  adminUser,
	}
}

func insertDashesUUID(hex32 string) string {
	return hex32[0:8] + "-" + hex32[8:12] + "-" + hex32[12:16] + "-" + hex32[16:20] + "-" + hex32[20:32]
}

func assertHex32(t *testing.T, id string) {
	t.Helper()
	if len(id) != 32 || strings.Contains(id, "-") {
		t.Fatalf("expected 32-char hex ID without dashes, got %q", id)
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("expected lowercase hex ID, got %q", id)
		}
	}
}

func authenticateTestUser(t *testing.T, router http.Handler) api.AuthenticationResult {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"Username": "admin",
		"Pw":       "secretpass",
	})
	req := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", `MediaBrowser Client="Jellyfin Web", Device="Firefox", DeviceId="dev-123", Version="10.9.0"`)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("AuthenticateByName status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var res api.AuthenticationResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode AuthenticationResult: %v", err)
	}
	return res
}

func TestLegacyPrefixStripping(t *testing.T) {
	env := newTestEnv(t)

	// /emby/System/Info/Public
	req := httptest.NewRequest(http.MethodGet, "/emby/System/Info/Public", nil)
	req.Host = "media.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /emby/System/Info/Public status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var pub struct {
		LocalAddress           string `json:"LocalAddress"`
		ServerName             string `json:"ServerName"`
		Version                string `json:"Version"`
		ProductName            string `json:"ProductName"`
		OperatingSystem        string `json:"OperatingSystem"`
		Id                     string `json:"Id"`
		StartupWizardCompleted bool   `json:"StartupWizardCompleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode public info: %v", err)
	}
	if pub.LocalAddress != "https://media.example.com" {
		t.Fatalf("LocalAddress = %q, want https://media.example.com", pub.LocalAddress)
	}
	if pub.Version != "12.4.0" || pub.ProductName != "Jellyfin Server" || !pub.StartupWizardCompleted {
		t.Fatalf("unexpected public info: %+v", pub)
	}
	assertHex32(t, pub.Id)

	// /mediabrowser/System/Ping (case-insensitive)
	req2 := httptest.NewRequest(http.MethodGet, "/MediaBrowser/System/Ping", nil)
	rec2 := httptest.NewRecorder()
	env.router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || rec2.Body.String() != "Jellyfin Server" {
		t.Fatalf("GET /MediaBrowser/System/Ping = %d %q", rec2.Code, rec2.Body.String())
	}
}

func TestAuthenticateByNameAndUserRoutes(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)

	assertHex32(t, authRes.AccessToken)
	assertHex32(t, authRes.ServerId)
	assertHex32(t, authRes.User.Id)
	assertHex32(t, authRes.SessionInfo.Id)
	assertHex32(t, authRes.SessionInfo.UserId)

	if authRes.User.Name != "admin" || !authRes.User.Policy.IsAdministrator {
		t.Fatalf("unexpected user dto: %+v", authRes.User)
	}
	if authRes.SessionInfo.Client != "Jellyfin Web" ||
		authRes.SessionInfo.DeviceName != "Firefox" ||
		authRes.SessionInfo.DeviceId != "dev-123" ||
		authRes.SessionInfo.ApplicationVersion != "10.9.0" {
		t.Fatalf("client metadata not extracted properly: %+v", authRes.SessionInfo)
	}
	if authRes.SessionInfo.PlayState == nil || authRes.SessionInfo.Capabilities == nil {
		t.Fatalf("expected non-nil PlayState and Capabilities in SessionInfo")
	}

	// Unauthenticated /Users/Me must fail with 401
	unauthReq := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	unauthRec := httptest.NewRecorder()
	env.router.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /Users/Me status = %d, want 401", unauthRec.Code)
	}

	// Authenticated /Users/Me
	meReq := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	meReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	meRec := httptest.NewRecorder()
	env.router.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("GET /Users/Me status = %d, body = %s", meRec.Code, meRec.Body.String())
	}
	var me api.UserDto
	if err := json.Unmarshal(meRec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode /Users/Me: %v", err)
	}
	if me.Id != authRes.User.Id || me.Name != "admin" {
		t.Fatalf("unexpected /Users/Me: %+v", me)
	}

	// /Users/{dashed-uuid} normalization
	dashed := insertDashesUUID(authRes.User.Id)
	byIDReq := httptest.NewRequest(http.MethodGet, "/Users/"+dashed, nil)
	byIDReq.Header.Set("X-MediaBrowser-Token", authRes.AccessToken)
	byIDRec := httptest.NewRecorder()
	env.router.ServeHTTP(byIDRec, byIDReq)
	if byIDRec.Code != http.StatusOK {
		t.Fatalf("GET /Users/{dashed} status = %d, body = %s", byIDRec.Code, byIDRec.Body.String())
	}
	var byID api.UserDto
	if err := json.Unmarshal(byIDRec.Body.Bytes(), &byID); err != nil {
		t.Fatalf("decode /Users/{dashed}: %v", err)
	}
	if byID.Id != authRes.User.Id {
		t.Fatalf("normalized ID mismatch: got %q, want %q", byID.Id, authRes.User.Id)
	}
}

func TestDisplayPreferencesRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)

	// Initial GET returns defaults
	getReq := httptest.NewRequest(http.MethodGet, "/DisplayPreferences/usersettings?client=emby", nil)
	getReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	getRec := httptest.NewRecorder()
	env.router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /DisplayPreferences/usersettings status = %d", getRec.Code)
	}
	var initial api.DisplayPreferencesDto
	if err := json.Unmarshal(getRec.Body.Bytes(), &initial); err != nil {
		t.Fatalf("decode initial DisplayPreferences: %v", err)
	}
	if initial.SortBy != "SortName" || initial.ViewType != "posters" || initial.CustomPrefs == nil {
		t.Fatalf("unexpected defaults: %+v", initial)
	}

	// POST update
	payload, _ := json.Marshal(map[string]any{
		"Id":       "usersettings",
		"ViewType": "list",
		"SortBy":   "DateCreated",
		"CustomPrefs": map[string]string{
			"homeSection0": "latestmedia",
		},
		"ShowBackdrop": false,
	})
	postReq := httptest.NewRequest(http.MethodPost, "/DisplayPreferences/usersettings?client=emby", bytes.NewReader(payload))
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	postRec := httptest.NewRecorder()
	env.router.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusNoContent {
		t.Fatalf("POST /DisplayPreferences/usersettings status = %d, body = %s", postRec.Code, postRec.Body.String())
	}

	// Second GET reflects persisted changes
	getReq2 := httptest.NewRequest(http.MethodGet, "/DisplayPreferences/usersettings?client=emby", nil)
	getReq2.Header.Set("X-Emby-Token", authRes.AccessToken)
	getRec2 := httptest.NewRecorder()
	env.router.ServeHTTP(getRec2, getReq2)
	if getRec2.Code != http.StatusOK {
		t.Fatalf("second GET status = %d", getRec2.Code)
	}
	var updated api.DisplayPreferencesDto
	if err := json.Unmarshal(getRec2.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated DisplayPreferences: %v", err)
	}
	if updated.ViewType != "list" || updated.SortBy != "DateCreated" || updated.ShowBackdrop != false || updated.CustomPrefs["homeSection0"] != "latestmedia" {
		t.Fatalf("unexpected updated DisplayPreferences: %+v", updated)
	}
}

func TestSystemInfoAndUtilityEndpoints(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)

	// Authenticated /System/Info
	req := httptest.NewRequest(http.MethodGet, "/System/Info", nil)
	req.Header.Set("X-Emby-Token", authRes.AccessToken)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /System/Info status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var info map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode /System/Info: %v", err)
	}
	if info["Version"] != "12.4.0" || info["SystemArchitecture"] != "X64" || info["SupportsLibraryMonitor"] != true {
		t.Fatalf("unexpected /System/Info payload: %+v", info)
	}

	// BitrateTest
	brReq := httptest.NewRequest(http.MethodGet, "/Playback/BitrateTest?Size=1234", nil)
	brRec := httptest.NewRecorder()
	env.router.ServeHTTP(brRec, brReq)
	if brRec.Code != http.StatusOK || brRec.Body.Len() != 1234 {
		t.Fatalf("BitrateTest = %d len=%d", brRec.Code, brRec.Body.Len())
	}

	// QuickConnect / Branding / Localization
	qcReq := httptest.NewRequest(http.MethodGet, "/QuickConnect/Enabled", nil)
	qcRec := httptest.NewRecorder()
	env.router.ServeHTTP(qcRec, qcReq)
	if qcRec.Code != http.StatusOK || strings.TrimSpace(qcRec.Body.String()) != "false" {
		t.Fatalf("QuickConnect = %d %q", qcRec.Code, qcRec.Body.String())
	}
}

func TestWebSocketHandshakeAndKeepAlive(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)

	srv := httptest.NewServer(env.router)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/socket?api_key=" + authRes.AccessToken
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.CloseNow()

	type wsMsg struct {
		MessageType string `json:"MessageType"`
		Data        any    `json:"Data"`
	}

	// Expect initial ForceKeepAlive + KeepAlive
	var seenForce, seenKeepAlive bool
	for i := 0; i < 2; i++ {
		var msg wsMsg
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			t.Fatalf("wsjson.Read #%d: %v", i, err)
		}
		switch msg.MessageType {
		case "ForceKeepAlive":
			seenForce = true
		case "KeepAlive":
			seenKeepAlive = true
		}
	}
	if !seenForce || !seenKeepAlive {
		t.Fatalf("expected ForceKeepAlive and KeepAlive on connect, got force=%v keepAlive=%v", seenForce, seenKeepAlive)
	}

	// Send KeepAlive and expect KeepAlive reply
	if err := wsjson.Write(ctx, conn, wsMsg{MessageType: "KeepAlive", Data: ""}); err != nil {
		t.Fatalf("wsjson.Write KeepAlive: %v", err)
	}

	var reply wsMsg
	if err := wsjson.Read(ctx, conn, &reply); err != nil {
		t.Fatalf("wsjson.Read reply: %v", err)
	}
	if reply.MessageType != "KeepAlive" {
		t.Fatalf("expected KeepAlive reply, got %+v", reply)
	}

	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestSessionLogoutCapabilitiesAndWebStatic(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)

	// Public users list
	pubReq := httptest.NewRequest(http.MethodGet, "/Users/Public", nil)
	pubRec := httptest.NewRecorder()
	env.router.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusOK {
		t.Fatalf("GET /Users/Public status = %d", pubRec.Code)
	}
	var pubUsers []api.UserDto
	if err := json.Unmarshal(pubRec.Body.Bytes(), &pubUsers); err != nil {
		t.Fatalf("decode /Users/Public: %v", err)
	}
	if len(pubUsers) != 1 || pubUsers[0].Name != "admin" {
		t.Fatalf("unexpected /Users/Public: %+v", pubUsers)
	}

	// Capabilities & Sessions
	for _, path := range []string{"/Sessions/Capabilities", "/Sessions/Capabilities/Full"} {
		capReq := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		capReq.Header.Set("X-Emby-Token", authRes.AccessToken)
		capRec := httptest.NewRecorder()
		env.router.ServeHTTP(capRec, capReq)
		if capRec.Code != http.StatusNoContent {
			t.Fatalf("POST %s status = %d", path, capRec.Code)
		}
	}

	sessReq := httptest.NewRequest(http.MethodGet, "/Sessions", nil)
	sessReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	sessRec := httptest.NewRecorder()
	env.router.ServeHTTP(sessRec, sessReq)
	if sessRec.Code != http.StatusOK {
		t.Fatalf("GET /Sessions status = %d", sessRec.Code)
	}

	// Logout invalidates token
	loReq := httptest.NewRequest(http.MethodPost, "/Sessions/Logout", nil)
	loReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	loRec := httptest.NewRecorder()
	env.router.ServeHTTP(loRec, loReq)
	if loRec.Code != http.StatusNoContent {
		t.Fatalf("POST /Sessions/Logout status = %d", loRec.Code)
	}

	meReq := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	meReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	meRec := httptest.NewRecorder()
	env.router.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /Users/Me after logout = %d, want 401", meRec.Code)
	}

	// Static web serving & root redirect
	webDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<h1>MergerFin Web</h1>"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "app.js"), []byte("console.log('ok');"), 0o644); err != nil {
		t.Fatalf("write app.js: %v", err)
	}
	webRouter := api.NewRouter(api.RouterDeps{
		Config: &config.Config{Port: "8096", ServerName: "MergerFin", WebDir: webDir},
		DB:     env.db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	redirReq := httptest.NewRequest(http.MethodGet, "/", nil)
	redirRec := httptest.NewRecorder()
	webRouter.ServeHTTP(redirRec, redirReq)
	if redirRec.Code != http.StatusFound || redirRec.Header().Get("Location") != "/web/index.html" {
		t.Fatalf("GET / redirect = %d location=%q", redirRec.Code, redirRec.Header().Get("Location"))
	}

	indexReq := httptest.NewRequest(http.MethodGet, "/web/", nil)
	indexRec := httptest.NewRecorder()
	webRouter.ServeHTTP(indexRec, indexReq)
	if indexRec.Code != http.StatusOK || !strings.Contains(indexRec.Body.String(), "MergerFin Web") {
		t.Fatalf("GET /web/ = %d body=%q", indexRec.Code, indexRec.Body.String())
	}

	assetReq := httptest.NewRequest(http.MethodGet, "/web/app.js", nil)
	assetRec := httptest.NewRecorder()
	webRouter.ServeHTTP(assetRec, assetReq)
	if assetRec.Code != http.StatusOK || !strings.Contains(assetRec.Body.String(), "console.log('ok')") {
		t.Fatalf("GET /web/app.js = %d body=%q", assetRec.Code, assetRec.Body.String())
	}
}

func TestCaseInsensitiveRoutingAndStaticPreservation(t *testing.T) {
	env := newTestEnv(t)

	// Lowercase GET /users/public (exact case jellyfin-web sends)
	pubReq := httptest.NewRequest(http.MethodGet, "/users/public", nil)
	pubRec := httptest.NewRecorder()
	env.router.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusOK {
		t.Fatalf("GET /users/public status = %d, body = %s", pubRec.Code, pubRec.Body.String())
	}

	// Mixed-case POST /Users/authenticatebyname (live login bug regression)
	body, _ := json.Marshal(map[string]string{
		"Username": "admin",
		"Pw":       "secretpass",
	})
	authReq := httptest.NewRequest(http.MethodPost, "/Users/authenticatebyname", bytes.NewReader(body))
	authReq.Header.Set("Content-Type", "application/json")
	authReq.Header.Set("Authorization", `MediaBrowser Client="Jellyfin Web", Device="Firefox", DeviceId="dev-ci", Version="10.9.0"`)
	authRec := httptest.NewRecorder()
	env.router.ServeHTTP(authRec, authReq)
	if authRec.Code != http.StatusOK {
		t.Fatalf("POST /Users/authenticatebyname status = %d, body = %s", authRec.Code, authRec.Body.String())
	}
	var authRes api.AuthenticationResult
	if err := json.Unmarshal(authRec.Body.Bytes(), &authRes); err != nil {
		t.Fatalf("decode /Users/authenticatebyname: %v", err)
	}

	// Mixed-case /SYSTEM/Info/Public
	sysReq := httptest.NewRequest(http.MethodGet, "/SYSTEM/Info/Public", nil)
	sysRec := httptest.NewRecorder()
	env.router.ServeHTTP(sysRec, sysReq)
	if sysRec.Code != http.StatusOK {
		t.Fatalf("GET /SYSTEM/Info/Public status = %d, body = %s", sysRec.Code, sysRec.Body.String())
	}

	// HEAD /admin/ and HEAD /admin
	headAdminSlashReq := httptest.NewRequest(http.MethodHead, "/admin/", nil)
	headAdminSlashRec := httptest.NewRecorder()
	env.router.ServeHTTP(headAdminSlashRec, headAdminSlashReq)
	if headAdminSlashRec.Code != http.StatusOK {
		t.Fatalf("HEAD /admin/ status = %d, want 200", headAdminSlashRec.Code)
	}

	headAdminReq := httptest.NewRequest(http.MethodHead, "/admin", nil)
	headAdminRec := httptest.NewRecorder()
	env.router.ServeHTTP(headAdminRec, headAdminReq)
	if headAdminRec.Code != http.StatusFound {
		t.Fatalf("HEAD /admin status = %d, want 302", headAdminRec.Code)
	}

	// /hls-proxy/ must preserve casing after "/hls-proxy/"
	var receivedPath string
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "text/vtt")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("WEBVTT\n\n"))
	}))
	defer upSrv.Close()

	pool, err := upstream.NewPool(env.db, "test-secret-key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	srvID := idgen.NewRandomHex32()
	encTok, err := pool.EncryptToken("up-tok")
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	ctx := context.Background()
	if err := env.db.UpsertServer(ctx, db.UpstreamServer{
		ID:         srvID,
		Name:       "UpHLS",
		BaseURL:    upSrv.URL,
		Username:   "u",
		AccessToken: encTok,
		Priority:   10,
		StreamMode: "proxy",
		Enabled:    true,
		IsOnline:   true,
	}); err != nil {
		t.Fatalf("UpsertServer: %v", err)
	}
	itemID := idgen.VirtualItemID("movie:hls-case")
	if err := env.db.UpsertItem(ctx, db.VirtualItem{
		ID:           itemID,
		LibraryID:    idgen.MoviesLibraryID(),
		ParentID:     idgen.MoviesLibraryID(),
		ItemType:     "Movie",
		CanonicalKey: "movie:hls-case",
		Name:         "HLS Case Movie",
	}); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	psID := idgen.NewRandomHex32()
	if err := env.db.CreatePlaySession(ctx, db.ActivePlaySession{
		PlaySessionID:       psID,
		UserID:              authRes.User.Id,
		DeviceID:            "dev-ci",
		VirtualItemID:       itemID,
		SourceID:            idgen.SourceID(srvID, "ms1"),
		ServerID:            srvID,
		RemoteItemID:        "rem-1",
		RemotePlaySessionID: "rem-ps-1",
		StartedAt:           time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreatePlaySession: %v", err)
	}

	hlsReq := httptest.NewRequest(http.MethodGet, "/Videos/"+strings.ToUpper(itemID)+"/hls-proxy/Subtitles/KeepCase.vtt?PlaySessionId="+psID, nil)
	hlsReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	hlsRec := httptest.NewRecorder()
	env.router.ServeHTTP(hlsRec, hlsReq)
	if hlsRec.Code != http.StatusOK {
		t.Fatalf("GET hls-proxy status = %d, body = %s", hlsRec.Code, hlsRec.Body.String())
	}
	if receivedPath != "/Subtitles/KeepCase.vtt" {
		t.Fatalf("upstream received path = %q, want %q", receivedPath, "/Subtitles/KeepCase.vtt")
	}

	// /favicon.ico returns 204 when WebDir has no favicon.ico, 200 when present
	fav204Req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	fav204Rec := httptest.NewRecorder()
	env.router.ServeHTTP(fav204Rec, fav204Req)
	if fav204Rec.Code != http.StatusNoContent {
		t.Fatalf("GET /favicon.ico without file = %d, want 204", fav204Rec.Code)
	}

	webDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "favicon.ico"), []byte("ico-bytes"), 0o644); err != nil {
		t.Fatalf("write favicon.ico: %v", err)
	}
	favRouter := api.NewRouter(api.RouterDeps{
		Config: &config.Config{Port: "8096", ServerName: "MergerFin", WebDir: webDir},
		DB:     env.db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	fav200Req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	fav200Rec := httptest.NewRecorder()
	favRouter.ServeHTTP(fav200Rec, fav200Req)
	if fav200Rec.Code != http.StatusOK || fav200Rec.Body.String() != "ico-bytes" {
		t.Fatalf("GET /favicon.ico with file = %d %q", fav200Rec.Code, fav200Rec.Body.String())
	}
}

func TestCompatEndpoints(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)
	ctx := context.Background()

	moviesLib := idgen.MoviesLibraryID()
	tvLib := idgen.TVShowsLibraryID()

	movieID := idgen.VirtualItemID("movie:compat-1")
	if err := env.db.UpsertItem(ctx, db.VirtualItem{
		ID:             movieID,
		LibraryID:      moviesLib,
		ParentID:       moviesLib,
		ItemType:       "Movie",
		CanonicalKey:   "movie:compat-1",
		Name:           "Inception",
		SortName:       "Inception",
		ProductionYear: 2010,
		RunTimeTicks:   88800000000,
		GenresJSON:     `["Action","Sci-Fi"]`,
	}); err != nil {
		t.Fatalf("UpsertItem movie: %v", err)
	}

	seriesID := idgen.VirtualItemID("series:compat-1")
	if err := env.db.UpsertItem(ctx, db.VirtualItem{
		ID:           seriesID,
		LibraryID:    tvLib,
		ParentID:     tvLib,
		ItemType:     "Series",
		CanonicalKey: "series:compat-1",
		Name:         "Severance",
		SortName:     "Severance",
		GenresJSON:   `["Drama","Sci-Fi"]`,
	}); err != nil {
		t.Fatalf("UpsertItem series: %v", err)
	}

	seasonID := idgen.VirtualItemID("season:compat-1:1")
	if err := env.db.UpsertItem(ctx, db.VirtualItem{
		ID:           seasonID,
		LibraryID:    tvLib,
		ParentID:     seriesID,
		SeriesID:     seriesID,
		ItemType:     "Season",
		CanonicalKey: "season:compat-1:1",
		Name:         "Season 1",
		SeriesName:   "Severance",
		IndexNumber:  1,
	}); err != nil {
		t.Fatalf("UpsertItem season: %v", err)
	}

	epID := idgen.VirtualItemID("ep:compat-1:1:1")
	if err := env.db.UpsertItem(ctx, db.VirtualItem{
		ID:                epID,
		LibraryID:         tvLib,
		ParentID:          seasonID,
		SeriesID:          seriesID,
		SeasonID:          seasonID,
		ItemType:          "Episode",
		CanonicalKey:      "ep:compat-1:1:1",
		Name:              "Good News About Hell",
		SeriesName:        "Severance",
		SeasonName:        "Season 1",
		IndexNumber:       1,
		ParentIndexNumber: 1,
		RunTimeTicks:      33000000000,
	}); err != nil {
		t.Fatalf("UpsertItem episode: %v", err)
	}

	if err := env.db.UpsertProgress(ctx, authRes.User.Id, movieID, 123456789, false, "2026-11-20T10:00:00Z"); err != nil {
		t.Fatalf("UpsertProgress: %v", err)
	}

	doAuthed := func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("X-Emby-Token", authRes.AccessToken)
		rec := httptest.NewRecorder()
		env.router.ServeHTTP(rec, req)
		return rec
	}

	// GET /useritems/resume
	resRec := doAuthed(http.MethodGet, "/useritems/resume")
	if resRec.Code != http.StatusOK {
		t.Fatalf("GET /useritems/resume = %d body=%s", resRec.Code, resRec.Body.String())
	}
	var resumeQR api.QueryResult
	if err := json.Unmarshal(resRec.Body.Bytes(), &resumeQR); err != nil {
		t.Fatalf("decode /useritems/resume: %v", err)
	}
	if resumeQR.TotalRecordCount != 1 || len(resumeQR.Items) != 1 || resumeQR.Items[0].Id != movieID {
		t.Fatalf("unexpected /useritems/resume: %+v", resumeQR)
	}

	// GET /useritems/{id}/userdata and /users/{userId}/items/{id}/userdata
	udRec := doAuthed(http.MethodGet, "/useritems/"+movieID+"/userdata")
	if udRec.Code != http.StatusOK {
		t.Fatalf("GET /useritems/{id}/userdata = %d body=%s", udRec.Code, udRec.Body.String())
	}
	var ud api.UserItemDataDto
	if err := json.Unmarshal(udRec.Body.Bytes(), &ud); err != nil {
		t.Fatalf("decode userdata: %v", err)
	}
	if ud.PlaybackPositionTicks != 123456789 || ud.Key != movieID {
		t.Fatalf("unexpected userdata: %+v", ud)
	}

	seriesUDRec := doAuthed(http.MethodGet, "/users/"+authRes.User.Id+"/items/"+seriesID+"/userdata")
	if seriesUDRec.Code != http.StatusOK {
		t.Fatalf("GET series userdata = %d", seriesUDRec.Code)
	}
	var seriesUD api.UserItemDataDto
	if err := json.Unmarshal(seriesUDRec.Body.Bytes(), &seriesUD); err != nil {
		t.Fatalf("decode series userdata: %v", err)
	}
	if seriesUD.UnplayedItemCount == nil || *seriesUD.UnplayedItemCount != 1 {
		t.Fatalf("expected UnplayedItemCount=1 for series, got %+v", seriesUD.UnplayedItemCount)
	}

	// POST & DELETE /useritems/{id}/rating
	for _, m := range []string{http.MethodPost, http.MethodDelete} {
		rateRec := doAuthed(m, "/useritems/"+movieID+"/rating")
		if rateRec.Code != http.StatusOK {
			t.Fatalf("%s /useritems/{id}/rating = %d", m, rateRec.Code)
		}
	}

	// GET /items/counts
	cntRec := doAuthed(http.MethodGet, "/items/counts")
	if cntRec.Code != http.StatusOK {
		t.Fatalf("GET /items/counts = %d body=%s", cntRec.Code, cntRec.Body.String())
	}
	var counts api.ItemCountsDto
	if err := json.Unmarshal(cntRec.Body.Bytes(), &counts); err != nil {
		t.Fatalf("decode /items/counts: %v", err)
	}
	if counts.MovieCount != 1 || counts.SeriesCount != 1 || counts.EpisodeCount != 1 || counts.ItemCount != 3 {
		t.Fatalf("unexpected counts: %+v", counts)
	}

	// GET /items/root
	rootRec := doAuthed(http.MethodGet, "/items/root")
	if rootRec.Code != http.StatusOK {
		t.Fatalf("GET /items/root = %d body=%s", rootRec.Code, rootRec.Body.String())
	}
	var rootDTO api.BaseItemDto
	if err := json.Unmarshal(rootRec.Body.Bytes(), &rootDTO); err != nil {
		t.Fatalf("decode /items/root: %v", err)
	}
	expectedRootID := idgen.DeterministicHex32("library:root")
	if rootDTO.Id != expectedRootID || rootDTO.Type != "AggregateFolder" || rootDTO.Name != "Media Folders" || !rootDTO.IsFolder || rootDTO.UserData.Key != expectedRootID {
		t.Fatalf("unexpected /items/root: %+v", rootDTO)
	}

	// GET /items/{id}/intros & other empty QueryResult endpoints
	for _, p := range []string{
		"/items/" + movieID + "/intros",
		"/users/" + authRes.User.Id + "/items/" + movieID + "/intros",
		"/items/" + movieID + "/themesongs",
		"/items/" + movieID + "/themevideos",
		"/shows/upcoming",
		"/studios",
		"/persons",
	} {
		rec := doAuthed(http.MethodGet, p)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", p, rec.Code)
		}
		var qr api.QueryResult
		if err := json.Unmarshal(rec.Body.Bytes(), &qr); err != nil || qr.Items == nil || qr.TotalRecordCount != 0 {
			t.Fatalf("unexpected empty QueryResult on %s: %s (err=%v)", p, rec.Body.String(), err)
		}
	}

	// GET /items/{id}/ancestors for Episode -> [Season, Series, Library]
	ancRec := doAuthed(http.MethodGet, "/items/"+epID+"/ancestors")
	if ancRec.Code != http.StatusOK {
		t.Fatalf("GET /items/{id}/ancestors = %d body=%s", ancRec.Code, ancRec.Body.String())
	}
	var ancestors []api.BaseItemDto
	if err := json.Unmarshal(ancRec.Body.Bytes(), &ancestors); err != nil {
		t.Fatalf("decode ancestors: %v", err)
	}
	if len(ancestors) != 3 || ancestors[0].Id != seasonID || ancestors[1].Id != seriesID || ancestors[2].Id != tvLib {
		t.Fatalf("unexpected ancestors order: %+v", ancestors)
	}

	// GET /items/{id}/thememedia
	tmRec := doAuthed(http.MethodGet, "/items/"+movieID+"/thememedia")
	if tmRec.Code != http.StatusOK {
		t.Fatalf("GET /items/{id}/thememedia = %d body=%s", tmRec.Code, tmRec.Body.String())
	}
	var tm api.ThemeMediaResult
	if err := json.Unmarshal(tmRec.Body.Bytes(), &tm); err != nil {
		t.Fatalf("decode thememedia: %v", err)
	}
	if tm.ThemeVideosResult.Items == nil || tm.ThemeSongsResult.Items == nil || tm.SoundtrackSongsResult.Items == nil {
		t.Fatalf("expected non-nil empty slices in ThemeMediaResult: %+v", tm)
	}

	// GET /genres & /genres/{genreName}
	genRec := doAuthed(http.MethodGet, "/genres")
	if genRec.Code != http.StatusOK {
		t.Fatalf("GET /genres = %d body=%s", genRec.Code, genRec.Body.String())
	}
	var genresQR api.QueryResult
	if err := json.Unmarshal(genRec.Body.Bytes(), &genresQR); err != nil {
		t.Fatalf("decode /genres: %v", err)
	}
	if genresQR.TotalRecordCount != 3 || len(genresQR.Items) != 3 {
		t.Fatalf("expected 3 genres (Action, Drama, Sci-Fi), got %+v", genresQR)
	}
	if genresQR.Items[0].Type != "Genre" || !genresQR.Items[0].IsFolder {
		t.Fatalf("unexpected genre item DTO: %+v", genresQR.Items[0])
	}

	gByNameRec := doAuthed(http.MethodGet, "/genres/Sci-Fi")
	if gByNameRec.Code != http.StatusOK {
		t.Fatalf("GET /genres/Sci-Fi = %d", gByNameRec.Code)
	}
	var gByName api.BaseItemDto
	if err := json.Unmarshal(gByNameRec.Body.Bytes(), &gByName); err != nil {
		t.Fatalf("decode /genres/Sci-Fi: %v", err)
	}
	if gByName.Name != "Sci-Fi" || gByName.Type != "Genre" {
		t.Fatalf("unexpected /genres/Sci-Fi: %+v", gByName)
	}

	// GET /search/hints
	shRec := doAuthed(http.MethodGet, "/search/hints?SearchTerm=Incept&IncludeItemTypes=Movie")
	if shRec.Code != http.StatusOK {
		t.Fatalf("GET /search/hints = %d body=%s", shRec.Code, shRec.Body.String())
	}
	var sh api.SearchHintsResult
	if err := json.Unmarshal(shRec.Body.Bytes(), &sh); err != nil {
		t.Fatalf("decode /search/hints: %v", err)
	}
	if sh.TotalRecordCount != 1 || len(sh.SearchHints) != 1 || sh.SearchHints[0].ItemId != movieID || sh.SearchHints[0].Name != "Inception" || sh.SearchHints[0].MediaType != "Video" {
		t.Fatalf("unexpected /search/hints: %+v", sh)
	}

	shEmptyRec := doAuthed(http.MethodGet, "/search/hints?SearchTerm=DoesNotExistXYZ")
	if shEmptyRec.Code != http.StatusOK || !strings.Contains(shEmptyRec.Body.String(), `"SearchHints":[]`) {
		t.Fatalf("expected non-nil empty SearchHints array, got %d %s", shEmptyRec.Code, shEmptyRec.Body.String())
	}

	// Miscellaneous compat endpoints
	for _, p := range []string{
		"/items/" + movieID + "/localtrailers",
		"/items/" + movieID + "/specialfeatures",
		"/userviews/groupingoptions",
		"/syncplay/list",
	} {
		rec := doAuthed(http.MethodGet, p)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("GET %s = %d %q, want 200 []", p, rec.Code, rec.Body.String())
		}
	}

	for _, p := range []string{
		"/users/configuration",
		"/users/" + authRes.User.Id + "/configuration",
		"/sessions/playing/ping",
		"/sessions/viewing",
		"/syncplay/pause",
	} {
		rec := doAuthed(http.MethodPost, p)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("POST %s = %d, want 204", p, rec.Code)
		}
	}

	ltRec := doAuthed(http.MethodGet, "/livetv/info")
	if ltRec.Code != http.StatusOK {
		t.Fatalf("GET /livetv/info = %d", ltRec.Code)
	}
	var lt api.LiveTVInfoDto
	if err := json.Unmarshal(ltRec.Body.Bytes(), &lt); err != nil || lt.IsEnabled || lt.Services == nil || lt.EnabledUsers == nil {
		t.Fatalf("unexpected /livetv/info: %+v err=%v", lt, err)
	}

	for _, p := range []string{"/shows/tvrecommended", "/shows/" + seriesID + "/similar"} {
		rec := doAuthed(http.MethodGet, p)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d body=%s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestDashboardAndPlayerCompatEndpoints(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)
	movieID := idgen.VirtualItemID("movie:dash-compat")

	doGet := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("X-Emby-Token", authRes.AccessToken)
		rec := httptest.NewRecorder()
		env.router.ServeHTTP(rec, req)
		return rec
	}

	// Public / mixed-case endpoints
	for _, p := range []string{
		"/web/ConfigurationPages",
		"/web/ConfigurationPage",
		"/ScheduledTasks",
		"/System/Logs",
	} {
		rec := doGet(p)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("GET %s = %d %q, want 200 []", p, rec.Code, rec.Body.String())
		}
	}

	// Empty QueryResult endpoints
	for _, p := range []string{
		"/System/ActivityLog/Entries",
		"/MediaSegments/" + movieID,
		"/Devices",
		"/Channels",
	} {
		rec := doGet(p)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d body=%s", p, rec.Code, rec.Body.String())
		}
		var qr api.QueryResult
		if err := json.Unmarshal(rec.Body.Bytes(), &qr); err != nil || qr.Items == nil || qr.TotalRecordCount != 0 {
			t.Fatalf("unexpected QueryResult on %s: %s (err=%v)", p, rec.Body.String(), err)
		}
	}

	// /System/Configuration -> {}
	cfgRec := doGet("/System/Configuration")
	if cfgRec.Code != http.StatusOK || strings.TrimSpace(cfgRec.Body.String()) != "{}" {
		t.Fatalf("GET /System/Configuration = %d %q, want 200 {}", cfgRec.Code, cfgRec.Body.String())
	}

	// /System/Configuration/Branding
	brandRec := doGet("/System/Configuration/Branding")
	if brandRec.Code != http.StatusOK || !strings.Contains(brandRec.Body.String(), "SplashscreenEnabled") {
		t.Fatalf("GET /System/Configuration/Branding = %d %s", brandRec.Code, brandRec.Body.String())
	}

	// /System/Info/Storage
	storRec := doGet("/System/Info/Storage")
	if storRec.Code != http.StatusOK {
		t.Fatalf("GET /System/Info/Storage = %d %s", storRec.Code, storRec.Body.String())
	}
	var stor api.SystemStorageDto
	if err := json.Unmarshal(storRec.Body.Bytes(), &stor); err != nil {
		t.Fatalf("decode /System/Info/Storage: %v", err)
	}
	if stor.CacheFolder.Path == "" || stor.DataFolder.Path == "" || stor.ImageCacheFolder.Path == "" ||
		stor.InternalMetadataFolder.Path == "" || stor.LogFolder.Path == "" || stor.TranscodingTempFolder.Path == "" ||
		stor.Libraries == nil {
		t.Fatalf("incomplete SystemStorageDto: %+v", stor)
	}

	// /Library/VirtualFolders
	vfRec := doGet("/Library/VirtualFolders")
	if vfRec.Code != http.StatusOK {
		t.Fatalf("GET /Library/VirtualFolders = %d %s", vfRec.Code, vfRec.Body.String())
	}
	var vfs []api.VirtualFolderInfoDto
	if err := json.Unmarshal(vfRec.Body.Bytes(), &vfs); err != nil {
		t.Fatalf("decode /Library/VirtualFolders: %v", err)
	}
	if len(vfs) != 2 {
		t.Fatalf("expected 2 virtual folders, got %d", len(vfs))
	}
	for _, vf := range vfs {
		assertHex32(t, vf.ItemId)
		if vf.Name == "" || vf.Locations == nil || vf.LibraryOptions == nil {
			t.Fatalf("invalid VirtualFolderInfoDto: %+v", vf)
		}
	}

	// /Library/MediaFolders
	mfRec := doGet("/Library/MediaFolders")
	if mfRec.Code != http.StatusOK {
		t.Fatalf("GET /Library/MediaFolders = %d %s", mfRec.Code, mfRec.Body.String())
	}
	var mf api.QueryResult
	if err := json.Unmarshal(mfRec.Body.Bytes(), &mf); err != nil {
		t.Fatalf("decode /Library/MediaFolders: %v", err)
	}
	if mf.TotalRecordCount != 2 || len(mf.Items) != 2 || mf.Items[0].Type != "CollectionFolder" {
		t.Fatalf("unexpected /Library/MediaFolders: %+v", mf)
	}
}

func TestDTOFieldsFilteringAndBatchedCounts(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)
	ctx := context.Background()

	moviesLib := idgen.MoviesLibraryID()
	tvLib := idgen.TVShowsLibraryID()

	movieID := idgen.VirtualItemID("movie:fields-1")
	if err := env.db.UpsertItem(ctx, db.VirtualItem{
		ID:           movieID,
		LibraryID:    moviesLib,
		ParentID:     moviesLib,
		ItemType:     "Movie",
		CanonicalKey: "movie:fields-1",
		Name:         "Blade Runner",
		SortName:     "Blade Runner",
		Overview:     "A blade runner must pursue and terminate four replicants.",
		PeopleJSON:   `[{"Name":"Harrison Ford","Role":"Rick Deckard","Type":"Actor"}]`,
		GenresJSON:   `["Sci-Fi"]`,
	}); err != nil {
		t.Fatalf("UpsertItem movie: %v", err)
	}
	srvID := idgen.NewRandomHex32()
	if err := env.db.UpsertServer(ctx, db.UpstreamServer{
		ID: srvID, Name: "UpFields", BaseURL: "http://127.0.0.1:8096", Username: "u",
		AccessToken: "tok", Priority: 10, StreamMode: "proxy", Enabled: true, IsOnline: true,
	}); err != nil {
		t.Fatalf("UpsertServer: %v", err)
	}
	for _, sid := range []string{"ms-1", "ms-2"} {
		if err := env.db.UpsertSource(ctx, db.ItemSource{
			SourceID:            idgen.SourceID(srvID, sid),
			VirtualItemID:       movieID,
			ServerID:            srvID,
			RemoteItemID:        "rem-1",
			RemoteMediaSourceID: sid,
			Label:               sid,
			RawMediaStreamsJSON: `[{"Type":"Video","Codec":"hevc","Index":0}]`,
		}); err != nil {
			t.Fatalf("UpsertSource: %v", err)
		}
	}

	seriesID := idgen.VirtualItemID("series:fields-1")
	seasonID := idgen.VirtualItemID("season:fields-1:1")
	ep1ID := idgen.VirtualItemID("ep:fields-1:1:1")
	ep2ID := idgen.VirtualItemID("ep:fields-1:1:2")
	for _, it := range []db.VirtualItem{
		{ID: seriesID, LibraryID: tvLib, ParentID: tvLib, ItemType: "Series", CanonicalKey: "series:fields-1", Name: "Andor", SortName: "Andor"},
		{ID: seasonID, LibraryID: tvLib, ParentID: seriesID, SeriesID: seriesID, ItemType: "Season", CanonicalKey: "season:fields-1:1", Name: "Season 1", IndexNumber: 1},
		{ID: ep1ID, LibraryID: tvLib, ParentID: seasonID, SeriesID: seriesID, SeasonID: seasonID, ItemType: "Episode", CanonicalKey: "ep:fields-1:1:1", Name: "Kassa", IndexNumber: 1, ParentIndexNumber: 1},
		{ID: ep2ID, LibraryID: tvLib, ParentID: seasonID, SeriesID: seriesID, SeasonID: seasonID, ItemType: "Episode", CanonicalKey: "ep:fields-1:1:2", Name: "That Would Be Me", IndexNumber: 2, ParentIndexNumber: 1},
	} {
		if err := env.db.UpsertItem(ctx, it); err != nil {
			t.Fatalf("UpsertItem TV: %v", err)
		}
	}
	if err := env.db.SetPlayed(ctx, authRes.User.Id, ep1ID, true, "2026-11-20T10:00:00Z"); err != nil {
		t.Fatalf("SetPlayed: %v", err)
	}

	// 1. Filtered list omits Overview, People, MediaSources while returning MediaSourceCount & non-null slices/maps.
	req := httptest.NewRequest(http.MethodGet, "/Items?IncludeItemTypes=Movie&Fields=PrimaryImageAspectRatio,MediaSourceCount", nil)
	req.Header.Set("X-Emby-Token", authRes.AccessToken)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /Items filtered = %d %s", rec.Code, rec.Body.String())
	}
	var qr api.QueryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &qr); err != nil {
		t.Fatalf("decode filtered /Items: %v", err)
	}
	if len(qr.Items) != 1 {
		t.Fatalf("expected 1 movie, got %d", len(qr.Items))
	}
	mv := qr.Items[0]
	if mv.MediaSourceCount != 2 {
		t.Fatalf("MediaSourceCount = %d, want 2", mv.MediaSourceCount)
	}
	if len(mv.MediaSources) != 0 {
		t.Fatalf("expected MediaSources omitted when not in Fields, got %+v", mv.MediaSources)
	}
	if mv.Overview != "" {
		t.Fatalf("expected Overview omitted when not in Fields, got %q", mv.Overview)
	}
	if mv.People == nil || len(mv.People) != 0 {
		t.Fatalf("expected non-nil empty People slice, got %#v", mv.People)
	}

	var rawMap map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rawMap)
	first := rawMap["Items"].([]any)[0].(map[string]any)
	for _, nonNullKey := range []string{"Genres", "GenreItems", "Studios", "People", "ImageTags", "BackdropImageTags", "ProviderIds", "RemoteTrailers", "LockedFields", "Tags", "ExternalUrls", "Taglines"} {
		if val, exists := first[nonNullKey]; !exists || val == nil {
			t.Fatalf("field %s must be present and non-null in filtered BaseItemDto", nonNullKey)
		}
	}
	if _, hasMediaSources := first["MediaSources"]; hasMediaSources {
		t.Fatalf("MediaSources JSON key must be omitted when not requested")
	}

	// 2. Single-item GET /Items/{id} remains full.
	fullReq := httptest.NewRequest(http.MethodGet, "/Items/"+movieID, nil)
	fullReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	fullRec := httptest.NewRecorder()
	env.router.ServeHTTP(fullRec, fullReq)
	if fullRec.Code != http.StatusOK {
		t.Fatalf("GET /Items/{id} = %d %s", fullRec.Code, fullRec.Body.String())
	}
	var fullDTO api.BaseItemDto
	_ = json.Unmarshal(fullRec.Body.Bytes(), &fullDTO)
	if fullDTO.Overview == "" || len(fullDTO.People) != 1 || len(fullDTO.MediaSources) != 2 || fullDTO.MediaSourceCount != 2 {
		t.Fatalf("expected full DTO on /Items/{id}, got overview=%q people=%d sources=%d count=%d",
			fullDTO.Overview, len(fullDTO.People), len(fullDTO.MediaSources), fullDTO.MediaSourceCount)
	}

	// 3. Series UnplayedItemCount in /Items list
	serReq := httptest.NewRequest(http.MethodGet, "/Items?IncludeItemTypes=Series&Fields=PrimaryImageAspectRatio,MediaSourceCount", nil)
	serReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	serRec := httptest.NewRecorder()
	env.router.ServeHTTP(serRec, serReq)
	if serRec.Code != http.StatusOK {
		t.Fatalf("GET /Items Series = %d %s", serRec.Code, serRec.Body.String())
	}
	var serQR api.QueryResult
	_ = json.Unmarshal(serRec.Body.Bytes(), &serQR)
	if len(serQR.Items) != 1 || serQR.Items[0].UserData.UnplayedItemCount == nil || *serQR.Items[0].UserData.UnplayedItemCount != 1 {
		t.Fatalf("expected Series UnplayedItemCount=1, got %+v", serQR.Items)
	}

	// 4. Explicit Fields=Overview,People,MediaSources returns all three
	explicitReq := httptest.NewRequest(http.MethodGet, "/Items?IncludeItemTypes=Movie&Fields=Overview,People,MediaSources", nil)
	explicitReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	explicitRec := httptest.NewRecorder()
	env.router.ServeHTTP(explicitRec, explicitReq)
	if explicitRec.Code != http.StatusOK {
		t.Fatalf("GET /Items explicit Fields = %d %s", explicitRec.Code, explicitRec.Body.String())
	}
	var expQR api.QueryResult
	_ = json.Unmarshal(explicitRec.Body.Bytes(), &expQR)
	if len(expQR.Items) != 1 || expQR.Items[0].Overview == "" || len(expQR.Items[0].People) != 1 || len(expQR.Items[0].MediaSources) != 2 || expQR.Items[0].MediaSourceCount != 2 {
		t.Fatalf("expected full fields when requested explicitly, got %+v", expQR.Items)
	}
}

func TestGzipCompressionAndStreamingBypass(t *testing.T) {
	env := newTestEnv(t)
	authRes := authenticateTestUser(t, env.router)
	ctx := context.Background()

	// 1. /Items with Accept-Encoding: gzip -> compressed JSON
	itemsReq := httptest.NewRequest(http.MethodGet, "/Items", nil)
	itemsReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	itemsReq.Header.Set("Accept-Encoding", "gzip")
	itemsRec := httptest.NewRecorder()
	env.router.ServeHTTP(itemsRec, itemsReq)
	if itemsRec.Code != http.StatusOK {
		t.Fatalf("GET /Items = %d", itemsRec.Code)
	}
	if ce := itemsRec.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("expected Content-Encoding: gzip on /Items, got %q", ce)
	}
	gz, err := gzip.NewReader(itemsRec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = gz.Close() }()
	decompressed, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read gzipped /Items: %v", err)
	}
	var qr api.QueryResult
	if err := json.Unmarshal(decompressed, &qr); err != nil {
		t.Fatalf("decode decompressed /Items: %v", err)
	}

	// 2. /Playback/BitrateTest must bypass gzip
	brReq := httptest.NewRequest(http.MethodGet, "/Playback/BitrateTest?Size=256", nil)
	brReq.Header.Set("Accept-Encoding", "gzip")
	brRec := httptest.NewRecorder()
	env.router.ServeHTTP(brRec, brReq)
	if brRec.Code != http.StatusOK || brRec.Header().Get("Content-Encoding") != "" || brRec.Body.Len() != 256 {
		t.Fatalf("BitrateTest gzip bypass failed: code=%d ce=%q len=%d", brRec.Code, brRec.Header().Get("Content-Encoding"), brRec.Body.Len())
	}

	// 3. /Videos/... must bypass gzip
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/vtt")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("WEBVTT\n\n"))
	}))
	defer upSrv.Close()

	pool, _ := upstream.NewPool(env.db, "test-secret-key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	srvID := idgen.NewRandomHex32()
	encTok, _ := pool.EncryptToken("up-tok")
	_ = env.db.UpsertServer(ctx, db.UpstreamServer{
		ID: srvID, Name: "UpGzip", BaseURL: upSrv.URL, Username: "u", AccessToken: encTok,
		Priority: 10, StreamMode: "proxy", Enabled: true, IsOnline: true,
	})
	itemID := idgen.VirtualItemID("movie:gzip-bypass")
	_ = env.db.UpsertItem(ctx, db.VirtualItem{
		ID: itemID, LibraryID: idgen.MoviesLibraryID(), ParentID: idgen.MoviesLibraryID(),
		ItemType: "Movie", CanonicalKey: "movie:gzip-bypass", Name: "Gzip Bypass",
	})
	psID := idgen.NewRandomHex32()
	_ = env.db.CreatePlaySession(ctx, db.ActivePlaySession{
		PlaySessionID: psID, UserID: authRes.User.Id, DeviceID: "dev-gz", VirtualItemID: itemID,
		SourceID: idgen.SourceID(srvID, "ms1"), ServerID: srvID, RemoteItemID: "rem-1",
		RemotePlaySessionID: "rem-ps-1", StartedAt: time.Now().UTC(),
	})

	vidReq := httptest.NewRequest(http.MethodGet, "/Videos/"+itemID+"/hls-proxy/sub.vtt?PlaySessionId="+psID, nil)
	vidReq.Header.Set("X-Emby-Token", authRes.AccessToken)
	vidReq.Header.Set("Accept-Encoding", "gzip")
	vidRec := httptest.NewRecorder()
	env.router.ServeHTTP(vidRec, vidReq)
	if vidRec.Code != http.StatusOK || vidRec.Header().Get("Content-Encoding") != "" || vidRec.Body.String() != "WEBVTT\n\n" {
		t.Fatalf("/Videos/ gzip bypass failed: code=%d ce=%q body=%q", vidRec.Code, vidRec.Header().Get("Content-Encoding"), vidRec.Body.String())
	}

	// 4. WebSocket /socket with Accept-Encoding: gzip must bypass compression and upgrade cleanly
	wsSrv := httptest.NewServer(env.router)
	defer wsSrv.Close()
	wsURL := "ws" + strings.TrimPrefix(wsSrv.URL, "http") + "/socket?api_key=" + authRes.AccessToken
	wsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(wsCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Accept-Encoding": []string{"gzip"}},
	})
	if err != nil {
		t.Fatalf("websocket.Dial with Accept-Encoding gzip failed: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "ok")
}

func TestImageWebPNegotiationPathParamsSingleflightAndCacheControl(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:", "ImgPerfTest")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := upstream.NewPool(database, "img-secret-key", logger)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	var hits atomic.Int32
	var lastQuery atomic.Value
	gate := make(chan struct{})
	var gateOnce sync.Once

	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastQuery.Store(r.URL.Query())
		if r.URL.Query().Get("maxWidth") == "512" {
			<-gate
		}
		w.Header().Set("Content-Type", "image/webp")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("RIFFmockwebp"))
	}))
	defer upSrv.Close()

	srvID := idgen.NewRandomHex32()
	encTok, _ := pool.EncryptToken("up-tok")
	_ = database.UpsertServer(ctx, db.UpstreamServer{
		ID: srvID, Name: "UpImg", BaseURL: upSrv.URL, Username: "u", AccessToken: encTok,
		Priority: 10, StreamMode: "proxy", Enabled: true, IsOnline: true,
	})

	movieID := idgen.VirtualItemID("movie:img-perf")
	_ = database.UpsertItem(ctx, db.VirtualItem{
		ID: movieID, LibraryID: idgen.MoviesLibraryID(), ParentID: idgen.MoviesLibraryID(),
		ItemType: "Movie", CanonicalKey: "movie:img-perf", Name: "Image Perf",
		ImgPrimaryServerID: srvID, ImgPrimaryRemoteID: "rem-img-1",
	})

	cfg := &config.Config{Port: "8096", DataDir: t.TempDir(), ServerName: "ImgPerf"}
	router := api.NewRouter(api.RouterDeps{
		Config: cfg,
		DB:     database,
		Pool:   pool,
		Client: upstream.NewClient(),
		Logger: logger,
	})

	// 1. fillWidth/fillHeight + Accept: image/webp + tag -> negotiates format=webp, strips tag upstream, sets immutable Cache-Control
	req1 := httptest.NewRequest(http.MethodGet, "/Items/"+movieID+"/Images/Primary?fillWidth=300&fillHeight=450&tag=virtualtag1", nil)
	req1.Header.Set("Accept", "image/avif,image/webp,*/*")
	req1.Header.Set("Accept-Encoding", "gzip")
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("image fillWidth status = %d body=%s", rec1.Code, rec1.Body.String())
	}
	if cc := rec1.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control with tag = %q, want immutable", cc)
	}
	if ce := rec1.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("expected /Images/ to bypass gzip, got Content-Encoding=%q", ce)
	}
	q1 := lastQuery.Load().(url.Values)
	if q1.Get("fillWidth") != "300" || q1.Get("fillHeight") != "450" || q1.Get("format") != "webp" || q1.Has("tag") {
		t.Fatalf("unexpected upstream query for WebP fillWidth: %v", q1)
	}

	// 2. No resize dimension + Accept: image/webp + no tag -> does NOT force WebP, Cache-Control is 604800
	req2 := httptest.NewRequest(http.MethodGet, "/Items/"+movieID+"/Images/Primary", nil)
	req2.Header.Set("Accept", "image/webp")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("unresized image status = %d", rec2.Code)
	}
	if cc := rec2.Header().Get("Cache-Control"); cc != "public, max-age=604800" {
		t.Fatalf("Cache-Control without tag = %q, want public, max-age=604800", cc)
	}
	q2 := lastQuery.Load().(url.Values)
	if q2.Get("format") != "" {
		t.Fatalf("expected no forced format without resize dim, got %v", q2)
	}

	// 3. Full positional path route ignoring "0" maxHeight and setting immutable Cache-Control from {tag}
	req3 := httptest.NewRequest(http.MethodGet, "/Items/"+movieID+"/Images/Primary/0/pathtag/png/400/0/0/0", nil)
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("positional image route status = %d body=%s", rec3.Code, rec3.Body.String())
	}
	if cc := rec3.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control with path tag = %q, want immutable", cc)
	}
	q3 := lastQuery.Load().(url.Values)
	if q3.Get("maxWidth") != "400" || q3.Get("format") != "png" || q3.Has("maxHeight") || q3.Has("tag") {
		t.Fatalf("unexpected upstream query for positional route: %v", q3)
	}

	// 4. singleflight coalescing: 10 concurrent misses for maxWidth=512 result in 1 upstream request
	hitsBefore := hits.Load()
	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	results := make([]int, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/Items/"+movieID+"/Images/Primary?maxWidth=512", nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			results[idx] = w.Code
			bodies[idx] = w.Body.String()
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	gateOnce.Do(func() { close(gate) })
	wg.Wait()

	for i := 0; i < n; i++ {
		if results[i] != http.StatusOK || bodies[i] != "RIFFmockwebp" {
			t.Fatalf("goroutine %d got status=%d body=%q", i, results[i], bodies[i])
		}
	}
	if delta := hits.Load() - hitsBefore; delta != 1 {
		t.Fatalf("expected singleflight to coalesce 10 concurrent misses into 1 upstream hit, got %d", delta)
	}
}
