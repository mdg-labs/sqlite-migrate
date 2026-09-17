# sqlite-migrate

A Go CLI + embeddable library that generates and safely applies SQLite
schema migrations from a plain-SQL `schema.sql` source of truth, including
full automatic rebuilds for changes SQLite's native `ALTER TABLE` can't
express.

This repository is under active development against the phased roadmap in
[`docs/internal/sqlite-migrate — Spec MVP Doc.md`](docs/internal/sqlite-migrate%20—%20Spec%20MVP%20Doc.md).
A full quickstart and CLI reference will land in Phase 9 once the `generate`,
`apply`, `check`, `verify`, and `status` commands exist.

## License

MIT — see [LICENSE](LICENSE).
