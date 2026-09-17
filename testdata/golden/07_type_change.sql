CREATE TABLE "users_sqlite_migrate_new" (
    id INTEGER PRIMARY KEY,
    age INTEGER
) STRICT;

INSERT INTO "users_sqlite_migrate_new" ("id", "age")
SELECT "id", "age"
FROM "users";

DROP TABLE "users";

ALTER TABLE "users_sqlite_migrate_new" RENAME TO "users";
