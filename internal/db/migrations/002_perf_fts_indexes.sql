DROP TRIGGER IF EXISTS virtual_items_ai;
DROP TRIGGER IF EXISTS virtual_items_au;
DROP TRIGGER IF EXISTS virtual_items_ad;
DROP TABLE IF EXISTS virtual_items_fts;

CREATE VIRTUAL TABLE IF NOT EXISTS virtual_items_fts USING fts5(
    item_id UNINDEXED,
    name,
    original_title,
    overview
);

INSERT INTO virtual_items_fts(rowid, item_id, name, original_title, overview)
SELECT rowid, id, name, original_title, overview FROM virtual_items;

CREATE TRIGGER IF NOT EXISTS virtual_items_ai AFTER INSERT ON virtual_items BEGIN
    INSERT INTO virtual_items_fts(rowid, item_id, name, original_title, overview)
    VALUES (NEW.rowid, NEW.id, NEW.name, NEW.original_title, NEW.overview);
END;

CREATE TRIGGER IF NOT EXISTS virtual_items_au AFTER UPDATE OF name, original_title, overview ON virtual_items
WHEN OLD.name != NEW.name OR OLD.original_title != NEW.original_title OR OLD.overview != NEW.overview BEGIN
    DELETE FROM virtual_items_fts WHERE rowid = OLD.rowid;
    INSERT INTO virtual_items_fts(rowid, item_id, name, original_title, overview)
    VALUES (NEW.rowid, NEW.id, NEW.name, NEW.original_title, NEW.overview);
END;

CREATE TRIGGER IF NOT EXISTS virtual_items_ad AFTER DELETE ON virtual_items BEGIN
    DELETE FROM virtual_items_fts WHERE rowid = OLD.rowid;
END;

CREATE INDEX IF NOT EXISTS idx_virtual_items_type_sort ON virtual_items(item_type, sort_name COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS idx_virtual_items_parent_sort ON virtual_items(parent_id, sort_name COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS idx_virtual_items_lib_date ON virtual_items(library_id, date_created DESC);
CREATE INDEX IF NOT EXISTS idx_user_item_data_user_played ON user_item_data(user_id, played, playback_position_ticks);
CREATE INDEX IF NOT EXISTS idx_item_sources_srv_remote_item ON item_sources(server_id, remote_item_id);
