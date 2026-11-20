package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"github.com/AL1X0/mergerfin/internal/idgen"
)

func TestReaderConcurrentReadsDuringOpenWriteTx(t *testing.T) {
	d, _ := openTestDB(t)
	ctx := context.Background()

	itemID := idgen.NewRandomHex32()
	if err := d.UpsertItem(ctx, VirtualItem{
		ID:              itemID,
		LibraryID:       idgen.MoviesLibraryID(),
		ParentID:        idgen.MoviesLibraryID(),
		ItemType:        "Movie",
		CanonicalKey:    "movie:concurrent:1",
		Name:            "Original Title",
		SortName:        "Original Title",
		GenresJSON:      "[]",
		StudiosJSON:     "[]",
		PeopleJSON:      "[]",
		ProviderIDsJSON: "{}",
	}); err != nil {
		t.Fatalf("seed UpsertItem: %v", err)
	}

	txStarted := make(chan struct{})
	releaseTx := make(chan struct{})
	txDone := make(chan error, 1)

	item2ID := idgen.NewRandomHex32()
	go func() {
		txDone <- d.WithWriteTx(ctx, func(tx *sql.Tx) error {
			if err := d.UpsertItemTx(ctx, tx, VirtualItem{
				ID:              item2ID,
				LibraryID:       idgen.MoviesLibraryID(),
				ParentID:        idgen.MoviesLibraryID(),
				ItemType:        "Movie",
				CanonicalKey:    "movie:concurrent:2",
				Name:            "Uncommitted Movie",
				SortName:        "Uncommitted Movie",
				GenresJSON:      "[]",
				StudiosJSON:     "[]",
				PeopleJSON:      "[]",
				ProviderIDsJSON: "{}",
			}); err != nil {
				return err
			}
			close(txStarted)
			<-releaseTx
			return nil
		})
	}()

	<-txStarted

	// Concurrent reads via readDB must not block or fail while the write tx is held open.
	var wg sync.WaitGroup
	errCh := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := d.GetItemByID(ctx, itemID)
			if err != nil {
				errCh <- fmt.Errorf("GetItemByID during write tx: %w", err)
				return
			}
			if got.Name != "Original Title" {
				errCh <- fmt.Errorf("unexpected item name %q", got.Name)
				return
			}
			items, total, err := d.QueryItems(ctx, ItemFilter{IncludeTypes: []string{"Movie"}})
			if err != nil {
				errCh <- fmt.Errorf("QueryItems during write tx: %w", err)
				return
			}
			if total != 1 || len(items) != 1 {
				errCh <- fmt.Errorf("expected 1 committed movie during open tx, got total=%d len=%d", total, len(items))
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		close(releaseTx)
		if txErr := <-txDone; txErr != nil {
			t.Errorf("tx error: %v", txErr)
		}
		t.Fatal(err)
	}

	close(releaseTx)
	if err := <-txDone; err != nil {
		t.Fatalf("WithWriteTx failed: %v", err)
	}

	// After commit, reader sees both movies.
	_, totalAfter, err := d.QueryItems(ctx, ItemFilter{IncludeTypes: []string{"Movie"}})
	if err != nil || totalAfter != 2 {
		t.Fatalf("expected total=2 after commit, got %d (err=%v)", totalAfter, err)
	}
}

func TestFTS5RowidTriggersUpdateDeleteAndBackfill(t *testing.T) {
	d, _ := openTestDB(t)
	ctx := context.Background()

	id1 := idgen.NewRandomHex32()
	item := VirtualItem{
		ID:              id1,
		LibraryID:       idgen.MoviesLibraryID(),
		ParentID:        idgen.MoviesLibraryID(),
		ItemType:        "Movie",
		CanonicalKey:    "movie:fts:1",
		Name:            "Blade Runner",
		SortName:        "Blade Runner",
		OriginalTitle:   "Blade Runner",
		Overview:        "Replicants in Los Angeles",
		GenresJSON:      "[]",
		StudiosJSON:     "[]",
		PeopleJSON:      "[]",
		ProviderIDsJSON: "{}",
	}
	if err := d.UpsertItem(ctx, item); err != nil {
		t.Fatalf("UpsertItem insert: %v", err)
	}

	// Verify initial FTS match.
	res, total, err := d.QueryItems(ctx, ItemFilter{SearchTerm: "Replicants"})
	if err != nil || total != 1 || len(res) != 1 || res[0].ID != id1 {
		t.Fatalf("initial FTS search failed: total=%d len=%d err=%v", total, len(res), err)
	}

	// Update FTS-indexed overview and verify old term is gone and new term matches.
	item.Overview = "Deckard hunts rogue androids"
	if err := d.UpsertItem(ctx, item); err != nil {
		t.Fatalf("UpsertItem update overview: %v", err)
	}
	_, oldTotal, err := d.QueryItems(ctx, ItemFilter{SearchTerm: "Replicants"})
	if err != nil || oldTotal != 0 {
		t.Fatalf("expected old FTS term to be removed on update, got total=%d err=%v", oldTotal, err)
	}
	res, newTotal, err := d.QueryItems(ctx, ItemFilter{SearchTerm: "androids"})
	if err != nil || newTotal != 1 || len(res) != 1 || res[0].ID != id1 {
		t.Fatalf("updated FTS search failed: total=%d len=%d err=%v", newTotal, len(res), err)
	}

	// Delete item and verify FTS entry is removed via rowid delete trigger.
	if _, err := d.SqlDB().ExecContext(ctx, `DELETE FROM virtual_items WHERE id = ?`, id1); err != nil {
		t.Fatalf("delete virtual_items: %v", err)
	}
	var ftsCount int
	if err := d.reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM virtual_items_fts WHERE item_id = ?`, id1).Scan(&ftsCount); err != nil {
		t.Fatalf("query virtual_items_fts: %v", err)
	}
	if ftsCount != 0 {
		t.Fatalf("expected 0 FTS rows after delete, got %d", ftsCount)
	}
}

func TestNoopUpsertSkipsWrites(t *testing.T) {
	d, _ := openTestDB(t)
	ctx := context.Background()

	itemID := idgen.NewRandomHex32()
	item := VirtualItem{
		ID:              itemID,
		LibraryID:       idgen.MoviesLibraryID(),
		ParentID:        idgen.MoviesLibraryID(),
		ItemType:        "Movie",
		CanonicalKey:    "movie:noop:1",
		Name:            "Arrival",
		SortName:        "Arrival",
		Overview:        "Linguist meets aliens",
		GenresJSON:      `["Sci-Fi"]`,
		StudiosJSON:     "[]",
		PeopleJSON:      "[]",
		ProviderIDsJSON: `{"Tmdb":"329865"}`,
	}

	var firstChanges, noopChanges, changedChanges int
	if err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if err := d.UpsertItemTx(ctx, tx, item); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT changes()`).Scan(&firstChanges); err != nil {
			return err
		}
		if err := d.UpsertItemTx(ctx, tx, item); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT changes()`).Scan(&noopChanges); err != nil {
			return err
		}
		item.Overview = "Updated overview"
		if err := d.UpsertItemTx(ctx, tx, item); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT changes()`).Scan(&changedChanges)
	}); err != nil {
		t.Fatalf("WithWriteTx item check: %v", err)
	}
	if firstChanges != 1 || noopChanges != 0 || changedChanges != 1 {
		t.Fatalf("unexpected item changes counts: first=%d noop=%d changed=%d", firstChanges, noopChanges, changedChanges)
	}

	srvID := idgen.NewRandomHex32()
	if err := d.UpsertServer(ctx, UpstreamServer{
		ID: srvID, Name: "S1", BaseURL: "http://example.local", Username: "u", Priority: 10, Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertServer: %v", err)
	}

	src := ItemSource{
		SourceID:            idgen.SourceID(srvID, "ms1"),
		VirtualItemID:       itemID,
		ServerID:            srvID,
		RemoteItemID:        "remote-1",
		RemoteMediaSourceID: "ms1",
		Label:               "[S1] 1080p",
		Container:           "mkv",
		RawMediaStreamsJSON: "[]",
		RawMediaSourceJSON:  "{}",
	}
	var srcFirst, srcNoop int
	if err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if err := d.UpsertSourceTx(ctx, tx, src); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT changes()`).Scan(&srcFirst); err != nil {
			return err
		}
		if err := d.UpsertSourceTx(ctx, tx, src); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT changes()`).Scan(&srcNoop)
	}); err != nil {
		t.Fatalf("WithWriteTx source check: %v", err)
	}
	if srcFirst != 1 || srcNoop != 0 {
		t.Fatalf("unexpected source changes counts: first=%d noop=%d", srcFirst, srcNoop)
	}

	ud := UserItemData{
		UserID:                "admin-1",
		VirtualItemID:         itemID,
		Played:                true,
		PlaybackPositionTicks: 500,
		PlayCount:             2,
		IsFavorite:            true,
		LastPlayedAt:          "2026-11-20T20:00:00Z",
	}
	var udFirst, udNoop, udLowerNoop int
	if err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if err := d.MergeInitialUserItemDataTx(ctx, tx, ud); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT changes()`).Scan(&udFirst); err != nil {
			return err
		}
		if err := d.MergeInitialUserItemDataTx(ctx, tx, ud); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT changes()`).Scan(&udNoop); err != nil {
			return err
		}
		lower := UserItemData{UserID: "admin-1", VirtualItemID: itemID, Played: false, PlaybackPositionTicks: 100, PlayCount: 1}
		if err := d.MergeInitialUserItemDataTx(ctx, tx, lower); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT changes()`).Scan(&udLowerNoop)
	}); err != nil {
		t.Fatalf("WithWriteTx userdata check: %v", err)
	}
	if udFirst != 1 || udNoop != 0 || udLowerNoop != 0 {
		t.Fatalf("unexpected userdata changes counts: first=%d noop=%d lowerNoop=%d", udFirst, udNoop, udLowerNoop)
	}
}

func TestBatchCountUnplayedInSubtreesAndCountSourcesByItemIDs(t *testing.T) {
	d, _ := openTestDB(t)
	ctx := context.Background()

	tvLib := idgen.TVShowsLibraryID()
	seriesID := idgen.VirtualItemID("series:batch-test")
	season1ID := idgen.VirtualItemID("season:batch-test:1")
	season2ID := idgen.VirtualItemID("season:batch-test:2")
	ep1ID := idgen.VirtualItemID("ep:batch-test:1:1")
	ep2ID := idgen.VirtualItemID("ep:batch-test:1:2")
	ep3ID := idgen.VirtualItemID("ep:batch-test:2:1")

	items := []VirtualItem{
		{ID: seriesID, LibraryID: tvLib, ParentID: tvLib, ItemType: "Series", CanonicalKey: "series:batch-test", Name: "Show", GenresJSON: "[]", StudiosJSON: "[]", PeopleJSON: "[]", ProviderIDsJSON: "{}"},
		{ID: season1ID, LibraryID: tvLib, ParentID: seriesID, SeriesID: seriesID, ItemType: "Season", CanonicalKey: "season:batch-test:1", Name: "Season 1", GenresJSON: "[]", StudiosJSON: "[]", PeopleJSON: "[]", ProviderIDsJSON: "{}"},
		{ID: season2ID, LibraryID: tvLib, ParentID: seriesID, SeriesID: seriesID, ItemType: "Season", CanonicalKey: "season:batch-test:2", Name: "Season 2", GenresJSON: "[]", StudiosJSON: "[]", PeopleJSON: "[]", ProviderIDsJSON: "{}"},
		{ID: ep1ID, LibraryID: tvLib, ParentID: season1ID, SeriesID: seriesID, SeasonID: season1ID, ItemType: "Episode", CanonicalKey: "ep:batch-test:1:1", Name: "E1", GenresJSON: "[]", StudiosJSON: "[]", PeopleJSON: "[]", ProviderIDsJSON: "{}"},
		{ID: ep2ID, LibraryID: tvLib, ParentID: season1ID, SeriesID: seriesID, SeasonID: "", ItemType: "Episode", CanonicalKey: "ep:batch-test:1:2", Name: "E2", GenresJSON: "[]", StudiosJSON: "[]", PeopleJSON: "[]", ProviderIDsJSON: "{}"},
		{ID: ep3ID, LibraryID: tvLib, ParentID: season2ID, SeriesID: seriesID, SeasonID: season2ID, ItemType: "Episode", CanonicalKey: "ep:batch-test:2:1", Name: "E3", GenresJSON: "[]", StudiosJSON: "[]", PeopleJSON: "[]", ProviderIDsJSON: "{}"},
	}
	for _, it := range items {
		if err := d.UpsertItem(ctx, it); err != nil {
			t.Fatalf("UpsertItem(%s): %v", it.ID, err)
		}
	}

	userID := "user-1"
	if err := d.SetPlayed(ctx, userID, ep1ID, true, "2026-11-20T10:00:00Z"); err != nil {
		t.Fatalf("SetPlayed(ep1): %v", err)
	}
	if err := d.SetPlayed(ctx, userID, ep3ID, true, "2026-11-20T11:00:00Z"); err != nil {
		t.Fatalf("SetPlayed(ep3): %v", err)
	}

	// Include >400 IDs and a dashed UUID to exercise normalization + chunking.
	dashedSeries := fmt.Sprintf("%s-%s-%s-%s-%s", seriesID[:8], seriesID[8:12], seriesID[12:16], seriesID[16:20], seriesID[20:])
	queryIDs := []string{dashedSeries, season1ID, season2ID}
	for i := 0; i < 450; i++ {
		queryIDs = append(queryIDs, idgen.DeterministicHex32(fmt.Sprintf("dummy-parent-%d", i)))
	}

	counts, err := d.BatchCountUnplayedInSubtrees(ctx, userID, queryIDs)
	if err != nil {
		t.Fatalf("BatchCountUnplayedInSubtrees: %v", err)
	}
	if counts[seriesID] != 1 {
		t.Fatalf("expected series unplayed=1, got %d", counts[seriesID])
	}
	if counts[season1ID] != 1 {
		t.Fatalf("expected season1 unplayed=1 (via parent_id fallback), got %d", counts[season1ID])
	}
	if counts[season2ID] != 0 {
		t.Fatalf("expected season2 unplayed=0, got %d", counts[season2ID])
	}
	if len(counts) != 453 {
		t.Fatalf("expected 453 keys initialized in result map, got %d", len(counts))
	}

	// Verify CountSourcesByItemIDs + LookupVirtualItemIDsByRemoteItem + GetAllItemsMap.
	srvID := idgen.NewRandomHex32()
	if err := d.UpsertServer(ctx, UpstreamServer{
		ID: srvID, Name: "Srv", BaseURL: "http://example.local", Username: "u", Priority: 10, Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertServer: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := d.UpsertSource(ctx, ItemSource{
			SourceID:            idgen.SourceID(srvID, fmt.Sprintf("ms-%d", i)),
			VirtualItemID:       ep1ID,
			ServerID:            srvID,
			RemoteItemID:        "remote-ep1",
			RemoteMediaSourceID: fmt.Sprintf("ms-%d", i),
			Label:               "src",
		}); err != nil {
			t.Fatalf("UpsertSource: %v", err)
		}
	}

	srcIDs := append([]string{ep1ID, ep2ID}, queryIDs[3:]...)
	srcCounts, err := d.CountSourcesByItemIDs(ctx, srcIDs)
	if err != nil {
		t.Fatalf("CountSourcesByItemIDs: %v", err)
	}
	if srcCounts[ep1ID] != 2 {
		t.Fatalf("expected 2 sources for ep1, got %d", srcCounts[ep1ID])
	}
	if srcCounts[ep2ID] != 0 {
		t.Fatalf("expected 0 sources for ep2, got %d", srcCounts[ep2ID])
	}

	vids, err := d.LookupVirtualItemIDsByRemoteItem(ctx, srvID, "remote-ep1")
	if err != nil || len(vids) != 1 || vids[0] != ep1ID {
		t.Fatalf("LookupVirtualItemIDsByRemoteItem: got %v err=%v", vids, err)
	}

	allMap, err := d.GetAllItemsMap(ctx)
	if err != nil || len(allMap) != 6 {
		t.Fatalf("GetAllItemsMap: expected 6 items, got %d (err=%v)", len(allMap), err)
	}
}

func TestMemorySharedCacheIsolation(t *testing.T) {
	ctx := context.Background()
	db1, err := Open(ctx, ":memory:", "DB1")
	if err != nil {
		t.Fatalf("Open db1: %v", err)
	}
	defer db1.Close()

	db2, err := Open(ctx, ":memory:", "DB2")
	if err != nil {
		t.Fatalf("Open db2: %v", err)
	}
	defer db2.Close()

	id1 := idgen.NewRandomHex32()
	if err := db1.UpsertItem(ctx, VirtualItem{
		ID: id1, LibraryID: idgen.MoviesLibraryID(), ParentID: idgen.MoviesLibraryID(),
		ItemType: "Movie", CanonicalKey: "mem:1", Name: "OnlyInDB1",
		GenresJSON: "[]", StudiosJSON: "[]", PeopleJSON: "[]", ProviderIDsJSON: "{}",
	}); err != nil {
		t.Fatalf("db1 UpsertItem: %v", err)
	}

	if got, err := db1.GetItemByID(ctx, id1); err != nil || got.Name != "OnlyInDB1" {
		t.Fatalf("db1 readDB did not see own write: got=%+v err=%v", got, err)
	}
	if _, err := db2.GetItemByID(ctx, id1); err == nil {
		t.Fatalf("db2 leaked item from db1 across :memory: instances")
	}
}
