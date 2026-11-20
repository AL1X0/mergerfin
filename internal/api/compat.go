package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/AL1X0/mergerfin/internal/auth"
	"github.com/AL1X0/mergerfin/internal/db"
	"github.com/AL1X0/mergerfin/internal/idgen"
)

// ItemCountsDto represents the Jellyfin /Items/Counts response payload.
type ItemCountsDto struct {
	MovieCount      int `json:"MovieCount"`
	SeriesCount     int `json:"SeriesCount"`
	EpisodeCount    int `json:"EpisodeCount"`
	ArtistCount     int `json:"ArtistCount"`
	ProgramCount    int `json:"ProgramCount"`
	TrailerCount    int `json:"TrailerCount"`
	SongCount       int `json:"SongCount"`
	AlbumCount      int `json:"AlbumCount"`
	MusicVideoCount int `json:"MusicVideoCount"`
	BoxSetCount     int `json:"BoxSetCount"`
	BookCount       int `json:"BookCount"`
	ItemCount       int `json:"ItemCount"`
}

// ThemeMediaResult represents the Jellyfin /Items/{itemId}/ThemeMedia response envelope.
type ThemeMediaResult struct {
	ThemeVideosResult     QueryResult `json:"ThemeVideosResult"`
	ThemeSongsResult      QueryResult `json:"ThemeSongsResult"`
	SoundtrackSongsResult QueryResult `json:"SoundtrackSongsResult"`
}

// SearchHintDto represents an individual entry in the Jellyfin /Search/Hints response.
type SearchHintDto struct {
	ItemId            string `json:"ItemId"`
	Id                string `json:"Id"`
	Name              string `json:"Name"`
	Type              string `json:"Type"`
	ProductionYear    int    `json:"ProductionYear,omitempty"`
	PrimaryImageTag   string `json:"PrimaryImageTag,omitempty"`
	Series            string `json:"Series,omitempty"`
	SeriesId          string `json:"SeriesId,omitempty"`
	IndexNumber       *int   `json:"IndexNumber,omitempty"`
	ParentIndexNumber *int   `json:"ParentIndexNumber,omitempty"`
	RunTimeTicks      int64  `json:"RunTimeTicks,omitempty"`
	MediaType         string `json:"MediaType,omitempty"`
}

// SearchHintsResult wraps SearchHints and TotalRecordCount for /Search/Hints.
type SearchHintsResult struct {
	SearchHints      []SearchHintDto `json:"SearchHints"`
	TotalRecordCount int             `json:"TotalRecordCount"`
}

// LiveTVInfoDto represents the Jellyfin /LiveTv/Info response payload.
type LiveTVInfoDto struct {
	Services     []any    `json:"Services"`
	IsEnabled    bool     `json:"IsEnabled"`
	EnabledUsers []string `json:"EnabledUsers"`
}

func (h *apiHandler) handleUserItemsResume(w http.ResponseWriter, r *http.Request) {
	h.handleGetResumeItems(w, r)
}

func (h *apiHandler) buildUserItemDataDTO(ctx context.Context, userID, itemID, itemType string) (UserItemDataDto, error) {
	ud, err := h.db.GetUserItemData(ctx, userID, itemID)
	if err != nil {
		return UserItemDataDto{}, err
	}
	dto := UserItemDataDto{
		PlaybackPositionTicks: ud.PlaybackPositionTicks,
		PlayCount:             ud.PlayCount,
		IsFavorite:            ud.IsFavorite,
		Played:                ud.Played,
		Key:                   itemID,
		LastPlayedDate:        ud.LastPlayedAt,
	}
	if itemType == "Series" || itemType == "Season" || itemType == "CollectionFolder" {
		if unplayed, cntErr := h.db.CountUnplayedInSubtree(ctx, userID, itemID); cntErr == nil {
			dto.UnplayedItemCount = &unplayed
		}
	}
	return dto, nil
}

func (h *apiHandler) resolveCompatUserAndItem(w http.ResponseWriter, r *http.Request) (userID, itemID, itemType string, ok bool) {
	ac, authOK := auth.FromContext(r.Context())
	if !authOK {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return "", "", "", false
	}

	itemID, itemOK := normalizeRouteID(chi.URLParam(r, "itemId"))
	if !itemOK {
		writeError(w, http.StatusBadRequest, "Invalid item ID")
		return "", "", "", false
	}

	userID = ac.User.ID
	if rawUID := strings.TrimSpace(chi.URLParam(r, "userId")); rawUID != "" {
		uid, uidOK := normalizeRouteID(rawUID)
		if !uidOK {
			writeError(w, http.StatusBadRequest, "Invalid user ID")
			return "", "", "", false
		}
		if uid != ac.User.ID && !ac.User.IsAdmin {
			writeError(w, http.StatusForbidden, "Forbidden")
			return "", "", "", false
		}
		userID = uid
	}

	ctx := r.Context()
	item, err := h.db.GetItemByID(ctx, itemID)
	if err == nil {
		return userID, itemID, item.ItemType, true
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "Failed to load item")
		return "", "", "", false
	}

	if _, libErr := h.db.GetLibraryByID(ctx, itemID); libErr == nil {
		return userID, itemID, "CollectionFolder", true
	}

	writeError(w, http.StatusNotFound, "Item not found")
	return "", "", "", false
}

func (h *apiHandler) handleGetUserItemUserData(w http.ResponseWriter, r *http.Request) {
	userID, itemID, itemType, ok := h.resolveCompatUserAndItem(w, r)
	if !ok {
		return
	}
	dto, err := h.buildUserItemDataDTO(r.Context(), userID, itemID, itemType)
	if err != nil {
		h.logger.Error("failed to get user item data", "user_id", userID, "item_id", itemID, "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to load user item data")
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

func (h *apiHandler) handleUserItemRating(w http.ResponseWriter, r *http.Request) {
	userID, itemID, itemType, ok := h.resolveCompatUserAndItem(w, r)
	if !ok {
		return
	}
	dto, err := h.buildUserItemDataDTO(r.Context(), userID, itemID, itemType)
	if err != nil {
		h.logger.Error("failed to get user item data for rating", "user_id", userID, "item_id", itemID, "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to load user item data")
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

func (h *apiHandler) handleItemsCounts(w http.ResponseWriter, r *http.Request) {
	if _, valid := resolveRequestUserID(r); !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	rows, err := h.db.SqlDB().QueryContext(r.Context(), `SELECT item_type, COUNT(*) FROM virtual_items GROUP BY item_type`)
	if err != nil {
		h.logger.Error("failed to count items", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to count items")
		return
	}
	defer rows.Close()

	var counts ItemCountsDto
	for rows.Next() {
		var itemType string
		var cnt int
		if err := rows.Scan(&itemType, &cnt); err != nil {
			h.logger.Error("failed to scan item count", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to count items")
			return
		}
		switch itemType {
		case "Movie":
			counts.MovieCount = cnt
		case "Series":
			counts.SeriesCount = cnt
		case "Episode":
			counts.EpisodeCount = cnt
		}
	}
	if err := rows.Err(); err != nil {
		h.logger.Error("item counts row error", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to count items")
		return
	}
	counts.ItemCount = counts.MovieCount + counts.SeriesCount + counts.EpisodeCount
	writeJSON(w, http.StatusOK, counts)
}

func newFolderBaseItemDTO(serverID, id, name, itemType string) BaseItemDto {
	return BaseItemDto{
		Name:              name,
		ServerId:          ensureHex32(serverID),
		Id:                id,
		Etag:              id,
		PlayAccess:        "Full",
		IsFolder:          true,
		Type:              itemType,
		LocationType:      "FileSystem",
		ExternalUrls:      []any{},
		Taglines:          []string{},
		Genres:            []string{},
		GenreItems:        []NameIDDto{},
		Studios:           []NameIDDto{},
		People:            []PersonDto{},
		ImageTags:         map[string]string{},
		BackdropImageTags: []string{},
		ProviderIds:       map[string]string{},
		RemoteTrailers:    []any{},
		LockedFields:      []string{},
		Tags:              []string{},
		UserData: UserItemDataDto{
			Key: id,
		},
	}
}

func (h *apiHandler) handleItemsRoot(w http.ResponseWriter, r *http.Request) {
	if _, valid := resolveRequestUserID(r); !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}
	serverID, _ := h.serverIdentity(r.Context())
	rootID := idgen.DeterministicHex32("library:root")
	writeJSON(w, http.StatusOK, newFolderBaseItemDTO(serverID, rootID, "Media Folders", "AggregateFolder"))
}

func (h *apiHandler) handleEmptyQueryResult(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, QueryResult{
		Items:            []BaseItemDto{},
		TotalRecordCount: 0,
		StartIndex:       0,
	})
}

func (h *apiHandler) handleThemeMedia(w http.ResponseWriter, r *http.Request) {
	empty := QueryResult{
		Items:            []BaseItemDto{},
		TotalRecordCount: 0,
		StartIndex:       0,
	}
	writeJSON(w, http.StatusOK, ThemeMediaResult{
		ThemeVideosResult:     empty,
		ThemeSongsResult:      empty,
		SoundtrackSongsResult: empty,
	})
}

func (h *apiHandler) handleItemAncestors(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, valid := resolveRequestUserID(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	itemID, ok := normalizeRouteID(chi.URLParam(r, "itemId"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid itemId")
		return
	}

	if itemID == idgen.DeterministicHex32("library:root") {
		writeJSON(w, http.StatusOK, []BaseItemDto{})
		return
	}

	item, err := h.db.GetItemByID(ctx, itemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if _, libErr := h.db.GetLibraryByID(ctx, itemID); libErr == nil {
				writeJSON(w, http.StatusOK, []BaseItemDto{})
				return
			}
			writeError(w, http.StatusNotFound, "item not found")
			return
		}
		h.logger.Error("get item ancestors failed", "item_id", itemID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load item ancestors")
		return
	}

	seen := map[string]bool{itemID: true}
	var parentItems []db.VirtualItem
	for _, rawParentID := range []string{item.SeasonID, item.SeriesID} {
		pid := ensureHex32(rawParentID)
		if pid == "" || seen[pid] {
			continue
		}
		seen[pid] = true
		if parent, err := h.db.GetItemByID(ctx, pid); err == nil {
			parentItems = append(parentItems, *parent)
		}
	}

	serverID, _ := h.serverIdentity(ctx)
	ancestors := make([]BaseItemDto, 0, len(parentItems)+1)
	if len(parentItems) > 0 {
		dtos, err := buildItemDTOs(ctx, h.db, serverID, userID, parentItems, false)
		if err != nil {
			h.logger.Error("build ancestor DTOs failed", "item_id", itemID, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to format item ancestors")
			return
		}
		ancestors = append(ancestors, dtos...)
	}

	libID := ensureHex32(item.LibraryID)
	if libID != "" && !seen[libID] {
		if lib, err := h.db.GetLibraryByID(ctx, libID); err == nil {
			ancestors = append(ancestors, libraryToDTO(ctx, h.db, serverID, userID, *lib))
		}
	}

	writeJSON(w, http.StatusOK, ancestors)
}

func (h *apiHandler) handleShowsTVRecommended(w http.ResponseWriter, r *http.Request) {
	h.handleGetNextUp(w, r)
}

func genreToDTO(serverID, name string) BaseItemDto {
	id := idgen.DeterministicHex32("genre:" + strings.ToLower(name))
	return newFolderBaseItemDTO(serverID, id, name, "Genre")
}

func (h *apiHandler) handleGenres(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, valid := resolveRequestUserID(r); !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	q := r.URL.Query()
	var parentID string
	if rawParent := queryVal(q, "ParentId", "parentId"); rawParent != "" {
		norm, ok := normalizeRouteID(rawParent)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid ParentId")
			return
		}
		parentID = norm
	}
	includeTypes := splitListParam(queryVal(q, "IncludeItemTypes", "includeItemTypes"))

	filters, err := h.db.GetFilters(ctx, parentID, includeTypes)
	if err != nil {
		h.logger.Error("get genres failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load genres")
		return
	}

	genres := filters.Genres
	total := len(genres)
	startIndex := max(0, parseOptionalInt(queryVal(q, "StartIndex", "startIndex"), 0))
	limit := max(0, parseOptionalInt(queryVal(q, "Limit", "limit"), 0))
	if startIndex < len(genres) {
		genres = genres[startIndex:]
		if limit > 0 && len(genres) > limit {
			genres = genres[:limit]
		}
	} else {
		genres = nil
	}

	serverID, _ := h.serverIdentity(ctx)
	dtos := make([]BaseItemDto, 0, len(genres))
	for _, g := range genres {
		dtos = append(dtos, genreToDTO(serverID, g))
	}

	writeJSON(w, http.StatusOK, QueryResult{
		Items:            dtos,
		TotalRecordCount: total,
		StartIndex:       startIndex,
	})
}

func (h *apiHandler) handleGenreByName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	genreName := strings.TrimSpace(chi.URLParam(r, "genreName"))
	if genreName == "" {
		writeError(w, http.StatusBadRequest, "invalid genreName")
		return
	}

	if filters, err := h.db.GetFilters(ctx, "", nil); err == nil {
		for _, g := range filters.Genres {
			if strings.EqualFold(g, genreName) {
				genreName = g
				break
			}
		}
	}

	serverID, _ := h.serverIdentity(ctx)
	writeJSON(w, http.StatusOK, genreToDTO(serverID, genreName))
}

func (h *apiHandler) handleSearchHints(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, valid := resolveRequestUserID(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	q := r.URL.Query()
	limit := parseOptionalInt(queryVal(q, "Limit", "limit"), 20)
	if limit <= 0 {
		limit = 20
	}
	startIndex := max(0, parseOptionalInt(queryVal(q, "StartIndex", "startIndex"), 0))

	filter := db.ItemFilter{
		UserID:       userID,
		SearchTerm:   queryVal(q, "SearchTerm", "searchTerm"),
		IncludeTypes: splitListParam(queryVal(q, "IncludeItemTypes", "includeItemTypes")),
		ExcludeTypes: splitListParam(queryVal(q, "ExcludeItemTypes", "excludeItemTypes")),
		StartIndex:   startIndex,
		Limit:        limit,
	}

	items, total, err := h.db.QueryItems(ctx, filter)
	if err != nil {
		h.logger.Error("search hints query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to search items")
		return
	}

	hints := make([]SearchHintDto, 0, len(items))
	for _, it := range items {
		itemID := ensureHex32(it.ID)
		var primaryTag string
		if it.ImgPrimaryServerID != "" && it.ImgPrimaryRemoteID != "" {
			primaryTag = idgen.DeterministicHex32("img:primary:" + it.ImgPrimaryServerID + ":" + it.ImgPrimaryRemoteID)
		}
		var mediaType string
		if it.ItemType == "Movie" || it.ItemType == "Episode" {
			mediaType = "Video"
		}
		var idxNum, parentIdxNum *int
		if it.ItemType == "Episode" || it.ItemType == "Season" || it.IndexNumber > 0 {
			v := it.IndexNumber
			idxNum = &v
		}
		if it.ItemType == "Episode" || it.ParentIndexNumber > 0 {
			v := it.ParentIndexNumber
			parentIdxNum = &v
		}

		hints = append(hints, SearchHintDto{
			ItemId:            itemID,
			Id:                itemID,
			Name:              it.Name,
			Type:              it.ItemType,
			ProductionYear:    it.ProductionYear,
			PrimaryImageTag:   primaryTag,
			Series:            it.SeriesName,
			SeriesId:          ensureHex32(it.SeriesID),
			IndexNumber:       idxNum,
			ParentIndexNumber: parentIdxNum,
			RunTimeTicks:      it.RunTimeTicks,
			MediaType:         mediaType,
		})
	}

	writeJSON(w, http.StatusOK, SearchHintsResult{
		SearchHints:      hints,
		TotalRecordCount: total,
	})
}

func (h *apiHandler) handleLiveTVInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, LiveTVInfoDto{
		Services:     []any{},
		IsEnabled:    false,
		EnabledUsers: []string{},
	})
}

func (h *apiHandler) handleEmptyObject(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{})
}

// FolderStorageDto represents storage metadata for a Jellyfin directory.
type FolderStorageDto struct {
	Path        string `json:"Path"`
	FreeSpace   int64  `json:"FreeSpace"`
	UsedSpace   int64  `json:"UsedSpace"`
	StorageType string `json:"StorageType"`
}

// SystemStorageDto represents the Jellyfin /System/Info/Storage response payload.
type SystemStorageDto struct {
	CacheFolder            FolderStorageDto `json:"CacheFolder"`
	DataFolder             FolderStorageDto `json:"DataFolder"`
	ProgramDataFolder      FolderStorageDto `json:"ProgramDataFolder"`
	ImageCacheFolder       FolderStorageDto `json:"ImageCacheFolder"`
	InternalMetadataFolder FolderStorageDto `json:"InternalMetadataFolder"`
	LogFolder              FolderStorageDto `json:"LogFolder"`
	TranscodingTempFolder  FolderStorageDto `json:"TranscodingTempFolder"`
	Libraries              []any            `json:"Libraries"`
}

func (h *apiHandler) handleSystemInfoStorage(w http.ResponseWriter, r *http.Request) {
	dataDir := "./data"
	if h.cfg != nil && strings.TrimSpace(h.cfg.DataDir) != "" {
		dataDir = strings.TrimSpace(h.cfg.DataDir)
	}
	folder := func(p string) FolderStorageDto {
		return FolderStorageDto{
			Path:        p,
			StorageType: "Local",
		}
	}
	writeJSON(w, http.StatusOK, SystemStorageDto{
		CacheFolder:            folder(filepath.Join(dataDir, "cache")),
		DataFolder:             folder(dataDir),
		ProgramDataFolder:      folder(dataDir),
		ImageCacheFolder:       folder(h.imageCacheDir()),
		InternalMetadataFolder: folder(filepath.Join(dataDir, "metadata")),
		LogFolder:              folder(filepath.Join(dataDir, "log")),
		TranscodingTempFolder:  folder(filepath.Join(dataDir, "transcodes")),
		Libraries:              []any{},
	})
}

// VirtualFolderInfoDto represents a virtual library entry for /Library/VirtualFolders.
type VirtualFolderInfoDto struct {
	Name           string         `json:"Name"`
	Locations      []string       `json:"Locations"`
	CollectionType string         `json:"CollectionType,omitempty"`
	LibraryOptions map[string]any `json:"LibraryOptions"`
	ItemId         string         `json:"ItemId"`
}

func (h *apiHandler) handleLibraryVirtualFolders(w http.ResponseWriter, r *http.Request) {
	libs, err := h.db.ListLibraries(r.Context())
	if err != nil {
		h.logger.Error("failed to list virtual folders", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list virtual folders")
		return
	}

	out := make([]VirtualFolderInfoDto, 0, len(libs))
	for _, lib := range libs {
		out = append(out, VirtualFolderInfoDto{
			Name:           lib.Name,
			Locations:      []string{},
			CollectionType: lib.CollectionType,
			LibraryOptions: map[string]any{},
			ItemId:         ensureHex32(lib.ID),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *apiHandler) handleLibraryMediaFolders(w http.ResponseWriter, r *http.Request) {
	userID, valid := resolveRequestUserID(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	libs, err := h.db.ListLibraries(r.Context())
	if err != nil {
		h.logger.Error("failed to list media folders", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list media folders")
		return
	}

	serverID, _ := h.serverIdentity(r.Context())
	items := make([]BaseItemDto, 0, len(libs))
	for _, lib := range libs {
		items = append(items, libraryToDTO(r.Context(), h.db, serverID, userID, lib))
	}

	writeJSON(w, http.StatusOK, QueryResult{
		Items:            items,
		TotalRecordCount: len(items),
		StartIndex:       0,
	})
}
