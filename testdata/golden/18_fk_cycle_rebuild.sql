CREATE TABLE "b_sqlite_migrate_new" (
    id INTEGER PRIMARY KEY,
    a_id INTEGER REFERENCES a(id),
    label INTEGER
) STRICT;

INSERT INTO "b_sqlite_migrate_new" ("id", "a_id", "label")
SELECT "id", "a_id", "label"
FROM "b";

CREATE TABLE "a_sqlite_migrate_new" (
    id INTEGER PRIMARY KEY,
    b_id INTEGER REFERENCES b(id),
    tag INTEGER
) STRICT;

INSERT INTO "a_sqlite_migrate_new" ("id", "b_id", "tag")
SELECT "id", "b_id", "tag"
FROM "a";

DROP TABLE "b";

ALTER TABLE "b_sqlite_migrate_new" RENAME TO "b";

DROP TABLE "a";

ALTER TABLE "a_sqlite_migrate_new" RENAME TO "a";
