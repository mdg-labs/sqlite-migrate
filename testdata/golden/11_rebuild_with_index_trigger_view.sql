DROP TRIGGER IF EXISTS "trg_items_updated";

DROP VIEW IF EXISTS "items_view";

CREATE TABLE "items_sqlite_migrate_new" (
    id INTEGER PRIMARY KEY,
    price INTEGER
) STRICT;

INSERT INTO "items_sqlite_migrate_new" ("id", "price")
SELECT "id", "price"
FROM "items";

DROP TABLE "items";

ALTER TABLE "items_sqlite_migrate_new" RENAME TO "items";

CREATE INDEX idx_items_price ON items(price);

CREATE VIEW items_view AS
SELECT id, price FROM items;

CREATE TRIGGER trg_items_updated
AFTER UPDATE ON items
BEGIN
    INSERT INTO items_audit (item_id, changed_at) VALUES (NEW.id, 'now');
END;
