package api

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/AL1X0/mergerfin/internal/admin"
	"github.com/AL1X0/mergerfin/internal/auth"
	"github.com/AL1X0/mergerfin/internal/config"
	"github.com/AL1X0/mergerfin/internal/db"
	"github.com/AL1X0/mergerfin/internal/stream"
	syncengine "github.com/AL1X0/mergerfin/internal/sync"
	"github.com/AL1X0/mergerfin/internal/upstream"
)

// RouterDeps holds all dependencies required to build the HTTP router.
type RouterDeps struct {
	Config     *config.Config
	DB         *db.DB
	Pool       *upstream.Pool
	Client     *upstream.Client
	Admin      *admin.Handler
	WSHub      *WSHub
	Streamer   *stream.Streamer
	Syncer     admin.SyncTrigger
	WatchState *syncengine.WatchStateManager
	Logger     *slog.Logger
}

// NewRouter constructs the root HTTP handler with prefix stripping, middlewares,
// Admin routes, WebSocket endpoint, static web assets, and Jellyfin 12.4 API routes.
func NewRouter(deps RouterDeps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if deps.Client == nil {
		deps.Client = upstream.NewClient()
	}
	if deps.Streamer == nil && deps.DB != nil && deps.Pool != nil {
		deps.Streamer = stream.NewStreamer(deps.DB, deps.Pool, logger)
	}

	h := newAPIHandler(deps.Config, deps.DB, deps.Pool, deps.Client, logger)
	h.wsHub = deps.WSHub
	if deps.WatchState != nil {
		h.watch = deps.WatchState
	} else if deps.DB != nil {
		var ws syncengine.Broadcaster
		if deps.WSHub != nil {
			ws = deps.WSHub
		}
		h.watch = syncengine.NewWatchStateManager(deps.DB, deps.Pool, deps.Client, ws, logger)
	}

	r := chi.NewRouter()
	r.Use(middleware.RealIP) //nolint:staticcheck // expected by Phase 2 router contract
	r.Use(slogAccessLogger(logger))
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS"},
		AllowedHeaders: []string{"*"},
		ExposedHeaders: []string{"Content-Range", "Accept-Ranges", "Content-Length"},
	}))
	r.Use(safeCompressMiddleware())

	if deps.Admin != nil {
		deps.Admin.Mount(r)
	}

	if deps.WSHub != nil {
		r.Get("/socket", deps.WSHub.HandleWebSocket)
		r.Get("/embywebsocket", deps.WSHub.HandleWebSocket)
	}

	serveFavicon := func(w http.ResponseWriter, r *http.Request) {
		if deps.Config != nil && strings.TrimSpace(deps.Config.WebDir) != "" {
			favPath := filepath.Join(deps.Config.WebDir, "favicon.ico")
			if info, err := os.Stat(favPath); err == nil && !info.IsDir() {
				http.ServeFile(w, r, favPath)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
	r.Get("/favicon.ico", serveFavicon)
	r.Head("/favicon.ico", serveFavicon)

	redirectToWeb := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/web/index.html", http.StatusFound)
	}
	r.Get("/", redirectToWeb)
	r.Get("/web", redirectToWeb)

	if deps.Config != nil && strings.TrimSpace(deps.Config.WebDir) != "" {
		if info, err := os.Stat(deps.Config.WebDir); err == nil && info.IsDir() {
			fileServer := http.StripPrefix("/web/", http.FileServer(http.Dir(deps.Config.WebDir)))
			r.Handle("/web/*", fileServer)
			logger.Info("serving static web assets", "dir", deps.Config.WebDir)
		}
	}

	// Public Jellyfin handshake, branding, localization, and auth endpoints.
	r.Get("/system/ping", h.handlePing)
	r.Post("/system/ping", h.handlePing)
	r.Get("/system/info/public", h.handleSystemInfoPublic)
	r.Get("/system/endpoint", h.handleSystemEndpoint)
	r.Get("/branding/configuration", h.handleBrandingConfiguration)
	r.Get("/system/configuration/branding", h.handleBrandingConfiguration)
	r.Get("/branding/css", h.handleBrandingCSS)
	r.Get("/branding/css.css", h.handleBrandingCSS)
	r.Get("/quickconnect/enabled", h.handleQuickConnectEnabled)
	r.Get("/plugins", h.handleEmptyArray)
	r.Get("/web/configurationpages", h.handleEmptyArray)
	r.Get("/web/configurationpage", h.handleEmptyArray)
	r.Get("/playback/bitratetest", h.handleBitrateTest)
	r.Get("/localization/options", h.handleEmptyArray)
	r.Get("/localization/countries", h.handleEmptyArray)
	r.Get("/localization/cultures", h.handleEmptyArray)
	r.Get("/localization/parentalratings", h.handleEmptyArray)
	r.Get("/users/public", h.handlePublicUsers)
	r.Post("/users/authenticatebyname", h.handleAuthenticateByName)

	// Public or optional-auth image routes (jellyfin-web/clients may pass ?api_key= or ?tag=).
	r.Group(func(ir chi.Router) {
		ir.Use(auth.OptionalUser(deps.DB))
		ir.Get("/items/{itemId}/images/{imageType}", h.handleGetItemImage)
		ir.Head("/items/{itemId}/images/{imageType}", h.handleGetItemImage)
		ir.Get("/items/{itemId}/images/{imageType}/{imageIndex}", h.handleGetItemImage)
		ir.Head("/items/{itemId}/images/{imageType}/{imageIndex}", h.handleGetItemImage)
		ir.Get("/items/{itemId}/images/{imageType}/{imageIndex}/{tag}/{format}/{maxWidth}/{maxHeight}/{percentPlayed}/{unplayedCount}", h.handleGetItemImage)
		ir.Head("/items/{itemId}/images/{imageType}/{imageIndex}/{tag}/{format}/{maxWidth}/{maxHeight}/{percentPlayed}/{unplayedCount}", h.handleGetItemImage)
	})

	// Authenticated Jellyfin endpoints.
	r.Group(func(ar chi.Router) {
		ar.Use(auth.Authenticate(deps.DB))
		ar.Get("/system/info", h.handleSystemInfo)
		ar.Get("/system/info/storage", h.handleSystemInfoStorage)
		ar.Get("/system/activitylog/entries", h.handleEmptyQueryResult)
		ar.Get("/system/logs", h.handleEmptyArray)
		ar.Get("/system/configuration", h.handleEmptyObject)
		ar.Get("/scheduledtasks", h.handleEmptyArray)
		ar.Get("/devices", h.handleEmptyQueryResult)
		ar.Get("/channels", h.handleEmptyQueryResult)
		ar.Get("/library/virtualfolders", h.handleLibraryVirtualFolders)
		ar.Get("/library/mediafolders", h.handleLibraryMediaFolders)
		ar.Get("/mediasegments/{itemId}", h.handleEmptyQueryResult)
		ar.Get("/users", h.handleUsers)
		ar.Get("/users/me", h.handleUserMe)
		ar.Get("/users/{userId}", h.handleUserByID)
		ar.Post("/users/configuration", h.handleNoContent)
		ar.Post("/users/{userId}/configuration", h.handleNoContent)
		ar.Get("/displaypreferences/{displayPreferencesId}", h.handleGetDisplayPreferences)
		ar.Post("/displaypreferences/{displayPreferencesId}", h.handlePostDisplayPreferences)
		ar.Post("/sessions/capabilities", h.handleNoContent)
		ar.Post("/sessions/capabilities/full", h.handleNoContent)
		ar.Post("/sessions/logout", h.handleLogout)
		ar.Get("/sessions", h.handleSessions)

		// Catalog, library views, and show endpoints (modern + legacy /users/{userId} routes).
		ar.Get("/userviews", h.handleUserViews)
		ar.Get("/views", h.handleUserViews)
		ar.Get("/users/{userId}/views", h.handleUserViews)
		ar.Get("/userviews/groupingoptions", h.handleEmptyArray)

		ar.Get("/items/latest", h.handleGetLatestItems)
		ar.Get("/users/{userId}/items/latest", h.handleGetLatestItems)
		ar.Get("/items/resume", h.handleGetResumeItems)
		ar.Get("/users/{userId}/items/resume", h.handleGetResumeItems)
		ar.Get("/useritems/resume", h.handleUserItemsResume)
		ar.Get("/useritems/{itemId}/userdata", h.handleGetUserItemUserData)
		ar.Get("/users/{userId}/items/{itemId}/userdata", h.handleGetUserItemUserData)
		ar.Post("/useritems/{itemId}/rating", h.handleUserItemRating)
		ar.Delete("/useritems/{itemId}/rating", h.handleUserItemRating)
		ar.Post("/users/{userId}/items/{itemId}/rating", h.handleUserItemRating)
		ar.Delete("/users/{userId}/items/{itemId}/rating", h.handleUserItemRating)
		ar.Get("/items/filters", h.handleGetFilters)
		ar.Get("/items/filters2", h.handleGetFilters)
		ar.Get("/items/counts", h.handleItemsCounts)
		ar.Get("/users/{userId}/items/counts", h.handleItemsCounts)
		ar.Get("/items/root", h.handleItemsRoot)
		ar.Get("/users/{userId}/items/root", h.handleItemsRoot)
		ar.Get("/items", h.handleGetItems)
		ar.Get("/users/{userId}/items", h.handleGetItems)
		ar.Get("/items/{itemId}", h.handleGetItemByID)
		ar.Get("/users/{userId}/items/{itemId}", h.handleGetItemByID)
		ar.Get("/items/{itemId}/similar", h.handleGetSimilarItems)
		ar.Get("/items/{itemId}/ancestors", h.handleItemAncestors)
		ar.Get("/users/{userId}/items/{itemId}/ancestors", h.handleItemAncestors)
		ar.Get("/items/{itemId}/intros", h.handleEmptyQueryResult)
		ar.Get("/users/{userId}/items/{itemId}/intros", h.handleEmptyQueryResult)
		ar.Get("/items/{itemId}/themesongs", h.handleEmptyQueryResult)
		ar.Get("/items/{itemId}/themevideos", h.handleEmptyQueryResult)
		ar.Get("/items/{itemId}/thememedia", h.handleThemeMedia)
		ar.Get("/users/{userId}/items/{itemId}/thememedia", h.handleThemeMedia)
		ar.Get("/items/{itemId}/localtrailers", h.handleEmptyArray)
		ar.Get("/users/{userId}/items/{itemId}/localtrailers", h.handleEmptyArray)
		ar.Get("/items/{itemId}/specialfeatures", h.handleEmptyArray)
		ar.Get("/users/{userId}/items/{itemId}/specialfeatures", h.handleEmptyArray)

		ar.Get("/shows/nextup", h.handleGetNextUp)
		ar.Get("/shows/tvrecommended", h.handleShowsTVRecommended)
		ar.Get("/shows/upcoming", h.handleEmptyQueryResult)
		ar.Get("/shows/{seriesId}/seasons", h.handleGetSeasons)
		ar.Get("/shows/{seriesId}/episodes", h.handleGetEpisodes)
		ar.Get("/shows/{itemId}/similar", h.handleGetSimilarItems)

		ar.Get("/genres", h.handleGenres)
		ar.Get("/genres/{genreName}", h.handleGenreByName)
		ar.Get("/studios", h.handleEmptyQueryResult)
		ar.Get("/persons", h.handleEmptyQueryResult)
		ar.Get("/search/hints", h.handleSearchHints)
		ar.Get("/livetv/info", h.handleLiveTVInfo)
		ar.Get("/syncplay/list", h.handleEmptyArray)
		ar.Post("/syncplay/{action}", h.handleNoContent)

		// Playback negotiation, active encodings cleanup, and media streaming.
		ar.Post("/items/{itemId}/playbackinfo", h.handlePlaybackInfo)
		ar.Get("/items/{itemId}/playbackinfo", h.handlePlaybackInfo)
		ar.Post("/users/{userId}/items/{itemId}/playbackinfo", h.handlePlaybackInfo)
		ar.Get("/users/{userId}/items/{itemId}/playbackinfo", h.handlePlaybackInfo)
		ar.Delete("/videos/activeencodings", h.handleStopActiveEncodings)
		if deps.Streamer != nil {
			ar.Get("/videos/{itemId}/stream", deps.Streamer.HandleDirectStream)
			ar.Head("/videos/{itemId}/stream", deps.Streamer.HandleDirectStream)
			ar.Get("/videos/{itemId}/stream.{ext}", deps.Streamer.HandleDirectStream)
			ar.Head("/videos/{itemId}/stream.{ext}", deps.Streamer.HandleDirectStream)
			ar.Get("/videos/{itemId}/master.m3u8", deps.Streamer.HandleHLSPlaylist)
			ar.Get("/videos/{itemId}/main.m3u8", deps.Streamer.HandleHLSPlaylist)
			ar.Get("/videos/{itemId}/live.m3u8", deps.Streamer.HandleHLSPlaylist)
			ar.Get("/videos/{itemId}/hls-proxy/*", deps.Streamer.HandleHLSProxy)
			ar.Head("/videos/{itemId}/hls-proxy/*", deps.Streamer.HandleHLSProxy)
		}

		// Playstate reporting and watched/favorite sync endpoints.
		ar.Post("/sessions/playing", h.handleSessionsPlaying)
		ar.Post("/sessions/playing/progress", h.handleSessionsPlayingProgress)
		ar.Post("/sessions/playing/stopped", h.handleSessionsPlayingStopped)
		ar.Post("/sessions/playing/ping", h.handleNoContent)
		ar.Post("/sessions/viewing", h.handleNoContent)
		ar.Post("/userplayeditems/{itemId}", h.handleMarkPlayed)
		ar.Delete("/userplayeditems/{itemId}", h.handleUnmarkPlayed)
		ar.Post("/users/{userId}/playeditems/{itemId}", h.handleMarkPlayed)
		ar.Delete("/users/{userId}/playeditems/{itemId}", h.handleUnmarkPlayed)
		ar.Post("/userfavoriteitems/{itemId}", h.handleMarkFavorite)
		ar.Delete("/userfavoriteitems/{itemId}", h.handleUnmarkFavorite)
		ar.Post("/users/{userId}/favoriteitems/{itemId}", h.handleMarkFavorite)
		ar.Delete("/users/{userId}/favoriteitems/{itemId}", h.handleUnmarkFavorite)
	})

	return normalizeRequestPath(r)
}

// normalizeRequestPath strips a leading case-insensitive /emby or /mediabrowser
// segment and lowercases Jellyfin API paths while preserving exact casing for
// static web/admin assets and upstream /hls-proxy/ subpaths.
func normalizeRequestPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		stripped := p

		switch {
		case len(p) >= len("/mediabrowser") && strings.EqualFold(p[:len("/mediabrowser")], "/mediabrowser") && (len(p) == len("/mediabrowser") || p[len("/mediabrowser")] == '/'):
			stripped = p[len("/mediabrowser"):]
		case len(p) >= len("/emby") && strings.EqualFold(p[:len("/emby")], "/emby") && (len(p) == len("/emby") || p[len("/emby")] == '/'):
			stripped = p[len("/emby"):]
		}
		if stripped == "" {
			stripped = "/"
		}

		lower := strings.ToLower(stripped)
		var normalized string
		switch {
		case lower == "/web/configurationpages" || lower == "/web/configurationpage":
			normalized = lower
		case lower == "/web" || strings.HasPrefix(lower, "/web/"):
			normalized = "/web" + stripped[len("/web"):]
		case lower == "/admin" || strings.HasPrefix(lower, "/admin/"):
			normalized = "/admin" + stripped[len("/admin"):]
		case strings.Contains(lower, "/hls-proxy/"):
			idx := strings.Index(lower, "/hls-proxy/")
			splitAt := idx + len("/hls-proxy/")
			if splitAt <= len(stripped) {
				normalized = lower[:splitAt] + stripped[splitAt:]
			} else {
				normalized = lower
			}
		default:
			normalized = lower
		}

		if normalized != p {
			r2 := r.Clone(r.Context())
			u := *r.URL
			u.Path = normalized
			u.RawPath = ""
			r2.URL = &u
			next.ServeHTTP(w, r2)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func shouldSkipCompression(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasPrefix(lower, "/videos/") ||
		strings.HasPrefix(lower, "/socket") ||
		strings.HasPrefix(lower, "/embywebsocket") ||
		strings.HasPrefix(lower, "/playback/bitratetest") ||
		strings.Contains(lower, "/images/")
}

func safeCompressMiddleware() func(http.Handler) http.Handler {
	compress := middleware.Compress(
		5,
		"application/json",
		"text/html",
		"text/css",
		"application/javascript",
		"text/javascript",
		"image/svg+xml",
	)
	return func(next http.Handler) http.Handler {
		compressedNext := compress(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if shouldSkipCompression(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			compressedNext.ServeHTTP(w, r)
		})
	}
}

func slogAccessLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			level := slog.LevelInfo
			if strings.EqualFold(r.URL.Path, "/System/Ping") {
				level = slog.LevelDebug
			}

			logger.Log(r.Context(), level, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration", time.Since(start),
				"remote_ip", r.RemoteAddr,
			)
		})
	}
}
