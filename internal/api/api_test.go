package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
