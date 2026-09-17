CREATE TABLE "foo$bar_sqlite_migrate_new" (
    id INTEGER PRIMARY KEY,
    v INTEGER CHECK (v >= 0)
) STRICT;

INSERT INTO "foo$bar_sqlite_migrate_new" ("id", "v")
SELECT "id", "v"
FROM "foo$bar";

DROP TABLE "foo$bar";

ALTER TABLE "foo$bar_sqlite_migrate_new" RENAME TO "foo$bar";
