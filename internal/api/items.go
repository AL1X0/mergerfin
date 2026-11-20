package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/AL1X0/mergerfin/internal/db"
	"github.com/AL1X0/mergerfin/internal/idgen"
)

func queryFirst(r *http.Request, keys ...string) string {
	return queryVal(r.URL.Query(), keys...)
}

func queryVal(q url.Values, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

func splitListParam(raw string, seps ...string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	sep := ","
	if len(seps) > 0 && strings.Contains(raw, seps[0]) {
		sep = seps[0]
	}
	parts := strings.Split(raw, sep)
	var out []string
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func parseOptionalBool(raw string) *bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1":
		b := true
		return &b
	case "false", "0":
		b := false
		return &b
	default:
		return nil
	}
}

func parseOptionalInt(raw string, def int) int {
	if strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return v
}

func (h *apiHandler) handleGetItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, valid := resolveRequestUserID(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}

	q := r.URL.Query()
	var filter db.ItemFilter
	filter.UserID = userID

	if rawIDs := queryVal(q, "Ids", "ids"); rawIDs != "" {
		parts := splitListParam(rawIDs)
		ids := make([]string, 0, len(parts))
		for _, p := range parts {
			norm, ok := normalizeRouteID(p)
			if !ok {
				writeError(w, http.StatusBadRequest, "invalid id in Ids parameter")
				return
			}
			ids = append(ids, norm)
		}
		filter.IDs = ids
	}

	if rawParent := queryVal(q, "ParentId", "parentId"); rawParent != "" {
		norm, ok := normalizeRouteID(rawParent)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid ParentId")
			return
		}
		filter.ParentID = norm
	}

	if rawTypes := queryVal(q, "IncludeItemTypes", "includeItemTypes"); rawTypes != "" {
		filter.IncludeTypes = splitListParam(rawTypes)
	}
	if rawExcl := queryVal(q, "ExcludeItemTypes", "excludeItemTypes"); rawExcl != "" {
		filter.ExcludeTypes = splitListParam(rawExcl)
	}

	filter.Recursive = strings.EqualFold(queryVal(q, "Recursive", "recursive"), "true")
	filter.SearchTerm = queryVal(q, "SearchTerm", "searchTerm")

	if rawGenres := queryVal(q, "Genres", "genres"); rawGenres != "" {
		if strings.Contains(rawGenres, "|") {
			filter.Genres = splitListParam(rawGenres, "|")
		} else {
			filter.Genres = splitListParam(rawGenres)
		}
	}

	if rawYears := queryVal(q, "Years", "years"); rawYears != "" {
		for _, ys := range splitListParam(rawYears) {
			if y, err := strconv.Atoi(ys); err == nil && y > 0 {
				filter.Years = append(filter.Years, y)
			}
		}
	}

	filter.IsPlayed = parseOptionalBool(queryVal(q, "IsPlayed", "isPlayed"))
	filter.IsFavorite = parseOptionalBool(queryVal(q, "IsFavorite", "isFavorite"))

	for _, f := range splitListParam(queryVal(q, "Filters", "filters")) {
		switch strings.ToLower(f) {
		case "isunplayed":
			b := false
			filter.IsPlayed = &b
		case "isplayed":
			b := true
			filter.IsPlayed = &b
		case "isfavorite":
			b := true
			filter.IsFavorite = &b
		}
	}

	if rawSort := queryVal(q, "SortBy", "sortBy"); rawSort != "" {
		parts := splitListParam(rawSort)
		if len(parts) > 0 {
			filter.SortBy = parts[0]
		}
	}
	filter.SortOrder = queryVal(q, "SortOrder", "sortOrder")
	filter.StartIndex = max(0, parseOptionalInt(queryVal(q, "StartIndex", "startIndex"), 0))
	filter.Limit = max(0, parseOptionalInt(queryVal(q, "Limit", "limit"), 0))

	serverID, _ := h.serverIdentity(ctx)

	// Handle direct library lookup via ?Ids=<libraryID> and resolve media source IDs to their parent virtual item.
	var libDTOs []BaseItemDto
	preferredSourceByItem := make(map[string]string)
	if len(filter.IDs) > 0 {
		resolvedIDs := make([]string, 0, len(filter.IDs))
		seenIDs := make(map[string]struct{}, len(filter.IDs))
		for _, id := range filter.IDs {
			if lib, err := h.db.GetLibraryByID(ctx, id); err == nil {
				libDTOs = append(libDTOs, libraryToDTO(ctx, h.db, serverID, userID, *lib))
				continue
			}
			virtID, srcID, _ := h.resolveItemOrSource(ctx, id)
			if srcID != "" {
				preferredSourceByItem[virtID] = srcID
			}
			if _, dup := seenIDs[virtID]; !dup {
				seenIDs[virtID] = struct{}{}
				resolvedIDs = append(resolvedIDs, virtID)
			}
		}
		filter.IDs = resolvedIDs
	}

	items, total, err := h.db.QueryItems(ctx, filter)
	if err != nil {
		h.logger.Error("query items failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to query items")
		return
	}

	dtos, err := buildItemDTOsWithFields(ctx, h.db, serverID, userID, items, true, queryVal(q, "Fields", "fields"))
	if err != nil {
		h.logger.Error("build item DTOs failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to format items")
		return
	}

	if len(preferredSourceByItem) > 0 {
		for i := range dtos {
			if prefSrc, ok := preferredSourceByItem[dtos[i].Id]; ok {
				prioritizeMediaSource(&dtos[i], prefSrc)
			}
		}
	}

	if len(libDTOs) > 0 {
		combined := make([]BaseItemDto, 0, len(libDTOs)+len(dtos))
		combined = append(combined, libDTOs...)
		combined = append(combined, dtos...)
		dtos = combined
		total += len(libDTOs)
	}

	writeJSON(w, http.StatusOK, QueryResult{
		Items:            dtos,
		TotalRecordCount: total,
		StartIndex:       filter.StartIndex,
	})
}

func (h *apiHandler) handleGetItemByID(w http.ResponseWriter, r *http.Request) {
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

	serverID, _ := h.serverIdentity(ctx)

	if lib, err := h.db.GetLibraryByID(ctx, itemID); err == nil {
		writeJSON(w, http.StatusOK, libraryToDTO(ctx, h.db, serverID, userID, *lib))
		return
	}

	var preferredSourceID string
	item, err := h.db.GetItemByID(ctx, itemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Jellyfin clients (notably jellyfin-web's version selector & playbackManager)
			// treat individual MediaSource.Id values as selectable item IDs.
			if src, srcErr := h.db.GetSourceByID(ctx, itemID); srcErr == nil && src != nil {
				preferredSourceID = src.SourceID
				item, err = h.db.GetItemByID(ctx, src.VirtualItemID)
			}
		}
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, "item not found")
				return
			}
			h.logger.Error("get item by id failed", "item_id", itemID, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to get item")
			return
		}
	}

	dtos, err := buildItemDTOs(ctx, h.db, serverID, userID, []db.VirtualItem{*item}, true)
	if err != nil || len(dtos) == 0 {
		h.logger.Error("build item dto failed", "item_id", itemID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to format item")
		return
	}

	if preferredSourceID != "" {
		prioritizeMediaSource(&dtos[0], preferredSourceID)
	}

	writeJSON(w, http.StatusOK, dtos[0])
}

// resolveItemOrSource resolves an ID that may be either a virtual_item_id or an item_sources.source_id.
func (h *apiHandler) resolveItemOrSource(ctx context.Context, id string) (virtualItemID string, matchedSourceID string, ok bool) {
	if _, err := h.db.GetItemByID(ctx, id); err == nil {
		return id, "", true
	}
	if src, err := h.db.GetSourceByID(ctx, id); err == nil && src != nil {
		return src.VirtualItemID, src.SourceID, true
	}
	return id, "", false
}

func prioritizeMediaSource(dto *BaseItemDto, sourceID string) {
	if dto == nil || sourceID == "" || len(dto.MediaSources) == 0 {
		return
	}
	idx := -1
	for i, ms := range dto.MediaSources {
		if ms.Id == sourceID {
			idx = i
			break
		}
	}
	if idx > 0 {
		selected := dto.MediaSources[idx]
		copy(dto.MediaSources[1:idx+1], dto.MediaSources[0:idx])
		dto.MediaSources[0] = selected
	}
	if idx >= 0 && dto.MediaSources[0].RunTimeTicks > 0 {
		dto.RunTimeTicks = dto.MediaSources[0].RunTimeTicks
	}
}

func (h *apiHandler) handleGetLatestItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, valid := resolveRequestUserID(r)
	if !valid {
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
	limit := parseOptionalInt(queryVal(q, "Limit", "limit"), 16)
	if limit <= 0 {
		limit = 16
	}

	items, err := h.db.GetLatestItems(ctx, userID, parentID, includeTypes, limit)
	if err != nil {
		h.logger.Error("get latest items failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to query latest items")
		return
	}

	serverID, _ := h.serverIdentity(ctx)
	dtos, err := buildItemDTOsWithFields(ctx, h.db, serverID, userID, items, true, queryVal(q, "Fields", "fields"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to format latest items")
		return
	}

	// Jellyfin /Items/Latest returns a raw JSON array, not QueryResult.
	writeJSON(w, http.StatusOK, dtos)
}

func (h *apiHandler) handleGetResumeItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, valid := resolveRequestUserID(r)
	if !valid {
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
	startIndex := max(0, parseOptionalInt(queryVal(q, "StartIndex", "startIndex"), 0))
	limit := parseOptionalInt(queryVal(q, "Limit", "limit"), 12)
	if limit <= 0 {
		limit = 12
	}

	items, err := h.db.GetResumeItems(ctx, userID, parentID, includeTypes, limit+startIndex)
	if err != nil {
		h.logger.Error("get resume items failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to query resume items")
		return
	}

	total := len(items)
	if startIndex < len(items) {
		items = items[startIndex:]
		if len(items) > limit {
			items = items[:limit]
		}
	} else {
		items = nil
	}

	serverID, _ := h.serverIdentity(ctx)
	dtos, err := buildItemDTOsWithFields(ctx, h.db, serverID, userID, items, true, queryVal(q, "Fields", "fields"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to format resume items")
		return
	}

	writeJSON(w, http.StatusOK, QueryResult{
		Items:            dtos,
		TotalRecordCount: total,
		StartIndex:       startIndex,
	})
}

func (h *apiHandler) handleGetFilters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
		h.logger.Error("get filters failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load filters")
		return
	}

	genres := filters.Genres
	if genres == nil {
		genres = []string{}
	}
	years := filters.Years
	if years == nil {
		years = []int{}
	}
	ratings := filters.OfficialRatings
	if ratings == nil {
		ratings = []string{}
	}

	if strings.HasSuffix(strings.ToLower(r.URL.Path), "filters2") {
		genreDTOs := make([]NameIDDto, 0, len(genres))
		for _, g := range genres {
			genreDTOs = append(genreDTOs, NameIDDto{
				Name: g,
				Id:   idgen.DeterministicHex32("genre:" + strings.ToLower(g)),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"Genres": genreDTOs,
			"Tags":   []string{},
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"Genres":          genres,
		"Tags":            []string{},
		"OfficialRatings": ratings,
		"Years":           years,
	})
}

func (h *apiHandler) handleGetSimilarItems(w http.ResponseWriter, r *http.Request) {
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

	limit := parseOptionalInt(queryFirst(r, "Limit", "limit"), 12)
	if limit <= 0 {
		limit = 12
	}

	items, err := h.db.GetSimilarItems(ctx, itemID, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "item not found")
			return
		}
		h.logger.Error("get similar items failed", "item_id", itemID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to query similar items")
		return
	}

	serverID, _ := h.serverIdentity(ctx)
	dtos, err := buildItemDTOsWithFields(ctx, h.db, serverID, userID, items, true, queryFirst(r, "Fields", "fields"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to format similar items")
		return
	}

	writeJSON(w, http.StatusOK, QueryResult{
		Items:            dtos,
		TotalRecordCount: len(dtos),
		StartIndex:       0,
	})
}
