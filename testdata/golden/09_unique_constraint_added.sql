CREATE TABLE "users_sqlite_migrate_new" (
    id INTEGER PRIMARY KEY,
    email TEXT NOT NULL UNIQUE
) STRICT;

INSERT INTO "users_sqlite_migrate_new" ("id", "email")
SELECT "id", "email"
FROM "users";

DROP TABLE "users";

ALTER TABLE "users_sqlite_migrate_new" RENAME TO "users";
