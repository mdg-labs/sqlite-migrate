CREATE TABLE "orders_sqlite_migrate_new" (
    id INTEGER PRIMARY KEY,
    user_id INTEGER REFERENCES users(id) ON DELETE CASCADE
) STRICT;

INSERT INTO "orders_sqlite_migrate_new" ("id", "user_id")
SELECT "id", "user_id"
FROM "orders";

DROP TABLE "orders";

ALTER TABLE "orders_sqlite_migrate_new" RENAME TO "orders";
