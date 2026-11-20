package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/AL1X0/mergerfin/internal/db"
	"github.com/AL1X0/mergerfin/internal/idgen"
	"github.com/AL1X0/mergerfin/internal/upstream"
	"golang.org/x/sync/singleflight"
)

var imgFlight singleflight.Group

type imageFetchResult struct {
	data        []byte
	contentType string
	status      int
	err         error
	notFoundMsg string
}

func (h *apiHandler) imageCacheDir() string {
	if h.cfg != nil && strings.TrimSpace(h.cfg.ImageCacheDir()) != "" {
		return h.cfg.ImageCacheDir()
	}
	return filepath.Join(os.TempDir(), "mergerfin-image-cache")
}

func buildImageCacheKey(itemID, imageType, imageIndex, maxWidth, maxHeight, width, height, fillWidth, fillHeight, quality, format, blur string) string {
	raw := strings.Join([]string{
		"v2",
		itemID,
		strings.ToLower(imageType),
		imageIndex,
		maxWidth,
		maxHeight,
		width,
		height,
		fillWidth,
		fillHeight,
		quality,
		strings.ToLower(format),
		blur,
	}, "|")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func writeFileAtomic(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%s", dst, idgen.NewRandomHex32())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func canonicalImageType(raw string) string {
	trimmed := strings.TrimSpace(raw)
	switch strings.ToLower(trimmed) {
	case "", "primary":
		return "Primary"
	case "backdrop":
		return "Backdrop"
	case "logo":
		return "Logo"
	case "thumb":
		return "Thumb"
	case "banner":
		return "Banner"
	case "art":
		return "Art"
	case "disc":
		return "Disc"
	default:
		runes := []rune(strings.ToLower(trimmed))
		runes[0] = unicode.ToUpper(runes[0])
		return string(runes)
	}
}

func normalizeImageDim(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" || v == "0" {
		return ""
	}
	return v
}

func imageCacheControl(r *http.Request) string {
	q := r.URL.Query()
	tag := firstNonEmpty(queryVal(q, "tag", "Tag"), chi.URLParam(r, "tag"))
	if tag != "" {
		return "public, max-age=31536000, immutable"
	}
	return "public, max-age=604800"
}

func (h *apiHandler) handleGetItemImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	itemID, ok := normalizeRouteID(chi.URLParam(r, "itemId"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid itemId")
		return
	}

	imageType := canonicalImageType(chi.URLParam(r, "imageType"))
	imageIndex := strings.TrimSpace(chi.URLParam(r, "imageIndex"))
	if imageIndex == "" {
		imageIndex = "0"
	}

	item, err := h.db.GetItemByID(ctx, itemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "item not found")
			return
		}
		h.logger.Error("lookup item for image failed", "item_id", itemID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to lookup item")
		return
	}

	var serverID, remoteID string
	upstreamType := imageType
	isSeasonOrEpisode := item.ItemType == "Season" || item.ItemType == "Episode"

	if imageType == "Backdrop" {
		if item.ImgBackdropServerID != "" && item.ImgBackdropRemoteID != "" {
			serverID = item.ImgBackdropServerID
			remoteID = item.ImgBackdropRemoteID
		} else {
			var series *db.VirtualItem
			if isSeasonOrEpisode && item.SeriesID != "" {
				series, _ = h.db.GetItemByID(ctx, item.SeriesID)
			}
			switch {
			case series != nil && series.ImgBackdropServerID != "" && series.ImgBackdropRemoteID != "":
				serverID = series.ImgBackdropServerID
				remoteID = series.ImgBackdropRemoteID
			case item.ImgPrimaryServerID != "" && item.ImgPrimaryRemoteID != "":
				serverID = item.ImgPrimaryServerID
				remoteID = item.ImgPrimaryRemoteID
				upstreamType = "Primary"
			case series != nil && series.ImgPrimaryServerID != "" && series.ImgPrimaryRemoteID != "":
				serverID = series.ImgPrimaryServerID
				remoteID = series.ImgPrimaryRemoteID
				upstreamType = "Primary"
			}
		}
	} else {
		serverID = item.ImgPrimaryServerID
		remoteID = item.ImgPrimaryRemoteID
		if (serverID == "" || remoteID == "") && isSeasonOrEpisode && item.SeriesID != "" {
			if series, sErr := h.db.GetItemByID(ctx, item.SeriesID); sErr == nil && series != nil {
				if series.ImgPrimaryServerID != "" && series.ImgPrimaryRemoteID != "" {
					serverID = series.ImgPrimaryServerID
					remoteID = series.ImgPrimaryRemoteID
				}
			}
		}
	}

	if serverID == "" || remoteID == "" {
		writeError(w, http.StatusNotFound, "image not available")
		return
	}

	q := r.URL.Query()
	maxWidth := normalizeImageDim(firstNonEmpty(queryVal(q, "maxWidth", "MaxWidth"), chi.URLParam(r, "maxWidth")))
	maxHeight := normalizeImageDim(firstNonEmpty(queryVal(q, "maxHeight", "MaxHeight"), chi.URLParam(r, "maxHeight")))
	width := normalizeImageDim(queryVal(q, "width", "Width"))
	height := normalizeImageDim(queryVal(q, "height", "Height"))
	fillWidth := normalizeImageDim(queryVal(q, "fillWidth", "FillWidth"))
	fillHeight := normalizeImageDim(queryVal(q, "fillHeight", "FillHeight"))
	quality := strings.TrimSpace(queryVal(q, "quality", "Quality"))
	format := strings.TrimSpace(firstNonEmpty(queryVal(q, "format", "Format"), chi.URLParam(r, "format")))
	blur := strings.TrimSpace(queryVal(q, "blur", "Blur"))
	backgroundColor := strings.TrimSpace(queryVal(q, "backgroundColor", "BackgroundColor"))
	foregroundLayer := strings.TrimSpace(queryVal(q, "foregroundLayer", "ForegroundLayer"))

	hasResizeDim := fillWidth != "" || fillHeight != "" || maxWidth != "" || maxHeight != "" || width != "" || height != ""
	if format == "" && hasResizeDim && strings.Contains(strings.ToLower(r.Header.Get("Accept")), "image/webp") {
		format = "webp"
	}

	cacheKey := buildImageCacheKey(
		itemID,
		imageType,
		imageIndex,
		maxWidth,
		maxHeight,
		width,
		height,
		fillWidth,
		fillHeight,
		quality,
		format,
		blur,
	)

	cacheDir := h.imageCacheDir()
	binPath := filepath.Join(cacheDir, cacheKey+".bin")
	ctPath := filepath.Join(cacheDir, cacheKey+".ct")

	etag := `"` + cacheKey + `"`
	if inm := strings.TrimSpace(r.Header.Get("If-None-Match")); inm != "" && inm == etag {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", imageCacheControl(r))
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Cache hit
	if data, err := os.ReadFile(binPath); err == nil && len(data) > 0 {
		ct := ""
		if ctBytes, ctErr := os.ReadFile(ctPath); ctErr == nil {
			ct = strings.TrimSpace(string(ctBytes))
		}
		if ct == "" {
			ct = http.DetectContentType(data)
		}
		serveCachedImage(w, r, ct, etag, data)
		return
	}

	// Cache miss: coalesce concurrent fetches for the same cacheKey via singleflight.
	if h.pool == nil {
		writeError(w, http.StatusServiceUnavailable, "upstream pool unavailable")
		return
	}

	upstreamQuery := url.Values{}
	if maxWidth != "" {
		upstreamQuery.Set("maxWidth", maxWidth)
	}
	if maxHeight != "" {
		upstreamQuery.Set("maxHeight", maxHeight)
	}
	if width != "" {
		upstreamQuery.Set("width", width)
	}
	if height != "" {
		upstreamQuery.Set("height", height)
	}
	if fillWidth != "" {
		upstreamQuery.Set("fillWidth", fillWidth)
	}
	if fillHeight != "" {
		upstreamQuery.Set("fillHeight", fillHeight)
	}
	if quality != "" {
		upstreamQuery.Set("quality", quality)
	}
	if format != "" {
		upstreamQuery.Set("format", format)
	}
	if blur != "" {
		upstreamQuery.Set("blur", blur)
	}
	if backgroundColor != "" {
		upstreamQuery.Set("backgroundColor", backgroundColor)
	}
	if foregroundLayer != "" {
		upstreamQuery.Set("foregroundLayer", foregroundLayer)
	}

	flightKey := cacheDir + "\x00" + cacheKey
	v, _, _ := imgFlight.Do(flightKey, func() (any, error) {
		if data, readErr := os.ReadFile(binPath); readErr == nil && len(data) > 0 {
			ct := ""
			if ctBytes, ctErr := os.ReadFile(ctPath); ctErr == nil {
				ct = strings.TrimSpace(string(ctBytes))
			}
			if ct == "" {
				ct = http.DetectContentType(data)
			}
			return &imageFetchResult{
				data:        data,
				contentType: ct,
				status:      http.StatusOK,
			}, nil
		}

		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		srv, srvErr := h.pool.GetDecryptedServer(fetchCtx, serverID)
		if srvErr != nil {
			if errors.Is(srvErr, sql.ErrNoRows) {
				return &imageFetchResult{status: http.StatusNotFound, err: srvErr, notFoundMsg: "upstream server not found"}, nil
			}
			h.logger.Error("decrypt upstream server for image failed", "server_id", serverID, "error", srvErr)
			return &imageFetchResult{status: http.StatusBadGateway, err: srvErr}, nil
		}

		client := h.client
		if client == nil {
			client = upstream.NewClient()
		}

		data, contentType, status, fetchErr := client.FetchImage(fetchCtx, *srv, remoteID, upstreamType, upstreamQuery)
		if fetchErr != nil {
			h.logger.Warn("fetch upstream image failed", "item_id", itemID, "server_id", serverID, "status", status, "error", fetchErr)
			return &imageFetchResult{status: status, err: fetchErr}, nil
		}
		if status != http.StatusOK || len(data) == 0 {
			return &imageFetchResult{status: status}, nil
		}

		if strings.TrimSpace(contentType) == "" {
			contentType = http.DetectContentType(data)
		}

		if ctWriteErr := writeFileAtomic(ctPath, []byte(contentType)); ctWriteErr != nil {
			h.logger.Warn("cache image content-type write failed", "error", ctWriteErr)
		}
		if binWriteErr := writeFileAtomic(binPath, data); binWriteErr != nil {
			h.logger.Warn("cache image payload write failed", "error", binWriteErr)
		}

		return &imageFetchResult{
			data:        data,
			contentType: contentType,
			status:      http.StatusOK,
		}, nil
	})

	res := v.(*imageFetchResult)
	if res.err != nil {
		if res.status == http.StatusNotFound {
			msg := res.notFoundMsg
			if msg == "" {
				msg = "upstream image not found"
			}
			writeError(w, http.StatusNotFound, msg)
			return
		}
		if res.notFoundMsg == "" && res.status == 0 {
			writeError(w, http.StatusBadGateway, "failed to fetch upstream image")
			return
		}
		writeError(w, http.StatusBadGateway, "upstream server unavailable")
		return
	}
	if res.status != http.StatusOK || len(res.data) == 0 {
		if res.status == http.StatusNotFound {
			writeError(w, http.StatusNotFound, "upstream image not found")
			return
		}
		writeError(w, http.StatusBadGateway, "upstream image request failed")
		return
	}

	serveCachedImage(w, r, res.contentType, etag, res.data)
}

func serveCachedImage(w http.ResponseWriter, r *http.Request, contentType, etag string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", imageCacheControl(r))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
