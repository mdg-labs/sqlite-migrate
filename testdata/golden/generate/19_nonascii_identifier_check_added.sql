CREATE TABLE "t_sqlite_migrate_new" (
    id INTEGER PRIMARY KEY,
    café INTEGER CHECK (café >= 0)
) STRICT;

INSERT INTO "t_sqlite_migrate_new" ("id", "café")
SELECT "id", "café"
FROM "t";

DROP TABLE "t";

ALTER TABLE "t_sqlite_migrate_new" RENAME TO "t";
