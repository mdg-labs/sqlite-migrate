CREATE TABLE items (
    id INTEGER PRIMARY KEY,
    price TEXT
) STRICT;

CREATE TABLE items_audit (
    id INTEGER PRIMARY KEY,
    item_id INTEGER,
    changed_at TEXT
) STRICT;

CREATE INDEX idx_items_price ON items(price);

CREATE TRIGGER trg_items_updated
AFTER UPDATE ON items
BEGIN
    INSERT INTO items_audit (item_id, changed_at) VALUES (NEW.id, 'now');
END;

CREATE VIEW items_view AS
SELECT id, price FROM items;
