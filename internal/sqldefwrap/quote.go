package sqldefwrap

import (
	"fmt"
	"strings"
	"sync"

	"github.com/sqldef/sqldef/v3/database"
	"github.com/sqldef/sqldef/v3/parser"

	"github.com/mdg-labs/sqlite-migrate/internal/sqlident"
)

// quoteCollidingKeywords double-quotes bare identifiers in CREATE TABLE
// text that sqldef's own SQLite parser rejects because they collide with a
// keyword its shared multi-dialect grammar reserves (serial, key, value,
// text, date, … — SQLite itself accepts every one of these as an ordinary
// identifier). Quoting only ever touches specific identifier positions —
// the table name, a column-definition's own name, the column lists inside
// PRIMARY KEY (…)/UNIQUE (…)/FOREIGN KEY (…), a REFERENCES target
// table/column list, and a CREATE [UNIQUE] INDEX's index name, table name
// and column list — found by walking the statement structure itself,
// never by scanning for a fixed word list. That keeps a column's own type
// keyword ("name TEXT") or an inline "PRIMARY KEY" past this pass
// untouched even though sqldef also rejects "text"/"key"/"primary" bare in
// other grammar positions, and leaves an identifier inside a CHECK/DEFAULT/
// GENERATED expression alone (out of scope: finding identifier position
// inside an arbitrary expression needs an expression-aware scanner, and
// sqldef's own syntax error there never produces a wrong migration).
func quoteCollidingKeywords(sql string) string {
	q := &keywordQuoter{els: tokenizeForKeywordQuoting(sql)}
	q.n = len(q.els)
	i := 0
	for i < q.n {
		if q.isIdent(i, "CREATE") {
			q.copy(i)
			i++
			i = q.afterCreate(i)
			continue
		}
		q.copy(i)
		i++
	}
	return q.out.String()
}

// identKeywordCache memoizes, per exact identifier spelling, whether
// sqldef's own SQLite parser rejects it as a bare identifier — the same
// mechanism the spec doc's scenario 22 row and issue #45 describe, so the
// set of colliding words follows sqldef's own keyword table across future
// version bumps instead of a hard-coded list going stale.
var identKeywordCache sync.Map // string -> bool

// sqldefRejectsIdentifier reports whether sqldef's SQLite parser refuses
// word as a bare column name, by asking the parser directly rather than
// consulting a hard-coded keyword list.
func sqldefRejectsIdentifier(word string) bool {
	if v, ok := identKeywordCache.Load(word); ok {
		return v.(bool)
	}
	probe := fmt.Sprintf("CREATE TABLE _sqlite_migrate_probe_ (%s INTEGER)", word)
	_, err := database.NewParser(parser.ParserModeSQLite3).Parse(probe)
	rejected := err != nil
	identKeywordCache.Store(word, rejected)
	return rejected
}

// qkElement is one token produced by tokenizeForKeywordQuoting: a bare
// identifier run ("ident"), a single whitespace byte ("ws"), a comment
// ("comment"), an already-quoted identifier/string/bracketed name
// ("quoted"), or any other single byte ("punct" — critically including
// '(', ')' and ',', the only bytes keywordQuoter's depth tracking cares
// about).
type qkElement struct {
	kind string
	text string
}

func tokenizeForKeywordQuoting(sql string) []qkElement {
	var els []qkElement
	n := len(sql)
	for i := 0; i < n; {
		c := sql[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := sqlident.ScanQuoted(sql, i, c)
			els = append(els, qkElement{"quoted", sql[i:j]})
			i = j
		case c == '[':
			j := strings.IndexByte(sql[i:], ']')
			if j < 0 {
				els = append(els, qkElement{"quoted", sql[i:]})
				i = n
			} else {
				els = append(els, qkElement{"quoted", sql[i : i+j+1]})
				i += j + 1
			}
		case c == '-' && i+1 < n && sql[i+1] == '-':
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				els = append(els, qkElement{"comment", sql[i:]})
				i = n
			} else {
				els = append(els, qkElement{"comment", sql[i : i+j+1]})
				i += j + 1
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			j := strings.Index(sql[i+2:], "*/")
			if j < 0 {
				els = append(els, qkElement{"comment", sql[i:]})
				i = n
			} else {
				end := i + 2 + j + 2
				els = append(els, qkElement{"comment", sql[i:end]})
				i = end
			}
		case sqlident.IsIdentByte(c):
			j := i + 1
			for j < n && sqlident.IsIdentByte(sql[j]) {
				j++
			}
			els = append(els, qkElement{"ident", sql[i:j]})
			i = j
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			els = append(els, qkElement{"ws", string(c)})
			i++
		default:
			els = append(els, qkElement{"punct", string(c)})
			i++
		}
	}
	return els
}

// keywordQuoter walks the token stream produced by tokenizeForKeywordQuoting,
// copying it to out unchanged except at the specific identifier positions
// documented on quoteCollidingKeywords, where a bare identifier
// sqldefRejectsIdentifier rejects is quoted instead.
type keywordQuoter struct {
	els []qkElement
	out strings.Builder
	n   int
}

func (q *keywordQuoter) copy(i int) { q.out.WriteString(q.els[i].text) }

func (q *keywordQuoter) isIdent(i int, word string) bool {
	return i < q.n && q.els[i].kind == "ident" && strings.EqualFold(q.els[i].text, word)
}

// skipWS copies past whitespace and comments, which can appear between any
// two significant tokens, and returns the index of the next significant
// one (or q.n).
func (q *keywordQuoter) skipWS(i int) int {
	for i < q.n && (q.els[i].kind == "ws" || q.els[i].kind == "comment") {
		q.copy(i)
		i++
	}
	return i
}

// quoteIdentAt copies els[i] to out, quoting it if it's a bare identifier
// sqldefRejectsIdentifier rejects; any other kind (already quoted, or i
// past the end) is copied unchanged. It returns the index past the token.
func (q *keywordQuoter) quoteIdentAt(i int) int {
	if i >= q.n {
		return i
	}
	if q.els[i].kind == "ident" {
		word := q.els[i].text
		if sqldefRejectsIdentifier(word) {
			q.out.WriteString(sqlident.QuoteIdent(word))
		} else {
			q.out.WriteString(word)
		}
		return i + 1
	}
	q.copy(i)
	return i + 1
}

func (q *keywordQuoter) afterCreate(i int) int {
	i = q.skipWS(i)
	if q.isIdent(i, "UNIQUE") {
		q.copy(i)
		i++
		i = q.skipWS(i)
		if !q.isIdent(i, "INDEX") {
			return i
		}
	}
	switch {
	case q.isIdent(i, "TABLE"):
		q.copy(i)
		i++
		return q.createTable(i)
	case q.isIdent(i, "INDEX"):
		q.copy(i)
		i++
		return q.createIndex(i)
	}
	return i
}

// ifNotExists copies an optional "IF NOT EXISTS" and returns the index of
// the significant token after it.
func (q *keywordQuoter) ifNotExists(i int) int {
	i = q.skipWS(i)
	if q.isIdent(i, "IF") {
		q.copy(i)
		i++
		i = q.skipWS(i)
		if q.isIdent(i, "NOT") {
			q.copy(i)
			i++
			i = q.skipWS(i)
		}
		if q.isIdent(i, "EXISTS") {
			q.copy(i)
			i++
			i = q.skipWS(i)
		}
	}
	return i
}

// createTable handles "[IF NOT EXISTS] <table-name> ( <column-list> )",
// quoting the table name and delegating the column list to columnList.
// Anything after the closing paren (STRICT, WITHOUT ROWID, the trailing
// ';') is left for the caller's own token loop.
func (q *keywordQuoter) createTable(i int) int {
	i = q.ifNotExists(i)
	i = q.quoteIdentAt(i)
	i = q.skipWS(i)
	if i < q.n && q.els[i].kind == "punct" && q.els[i].text == "(" {
		q.copy(i)
		i++
		i = q.columnList(i)
	}
	return i
}

// createIndex handles "[IF NOT EXISTS] <index-name> ON <table-name>
// ( <indexed-columns> )", quoting the index name, the table name and each
// indexed column. A trailing partial-index WHERE clause is left for the
// caller's own token loop, like anything after CREATE TABLE's column list.
func (q *keywordQuoter) createIndex(i int) int {
	i = q.ifNotExists(i)
	i = q.quoteIdentAt(i)
	i = q.skipWS(i)
	if !q.isIdent(i, "ON") {
		return i
	}
	q.copy(i)
	i++
	i = q.skipWS(i)
	i = q.quoteIdentAt(i)
	i = q.skipWS(i)
	if i < q.n && q.els[i].kind == "punct" && q.els[i].text == "(" {
		q.copy(i)
		i++
		i = q.identList(i)
	}
	return i
}

// columnList walks the comma-separated body of a CREATE TABLE at depth 1
// (the '(' that opens it was already consumed by the caller), dispatching
// each entry to tableConstraint or a column definition, until the matching
// closing ')' returns depth to 0.
func (q *keywordQuoter) columnList(i int) int {
	depth := 1
	defStart := true
	for i < q.n && depth > 0 {
		i = q.skipWS(i)
		if i >= q.n {
			break
		}
		el := q.els[i]

		if defStart {
			defStart = false
			if el.kind == "ident" && isTableConstraintLead(el.text) {
				i = q.tableConstraint(i)
				continue
			}
			i = q.quoteIdentAt(i)
			i = q.columnBody(i)
			continue
		}

		switch {
		case el.kind == "punct" && el.text == "(":
			depth++
			q.copy(i)
			i++
		case el.kind == "punct" && el.text == ")":
			depth--
			q.copy(i)
			i++
		case el.kind == "punct" && el.text == "," && depth == 1:
			q.copy(i)
			i++
			defStart = true
		default:
			q.copy(i)
			i++
		}
	}
	return i
}

func isTableConstraintLead(word string) bool {
	switch strings.ToUpper(word) {
	case "CONSTRAINT", "PRIMARY", "UNIQUE", "CHECK", "FOREIGN":
		return true
	}
	return false
}

// tableConstraint dispatches a table-level constraint clause (the current
// token is one of CONSTRAINT/PRIMARY/UNIQUE/CHECK/FOREIGN, already
// confirmed by isTableConstraintLead) to the handler for its kind. A named
// constraint's name is copied verbatim — it isn't one of the positions
// #45 lists for quoting — and dispatch continues on the keyword after it.
func (q *keywordQuoter) tableConstraint(i int) int {
	if q.isIdent(i, "CONSTRAINT") {
		q.copy(i)
		i++
		i = q.skipWS(i)
		if i < q.n {
			q.copy(i)
			i++
		}
		i = q.skipWS(i)
		return q.tableConstraint(i)
	}
	switch {
	case q.isIdent(i, "PRIMARY"):
		return q.primaryOrUnique(i, "PRIMARY")
	case q.isIdent(i, "UNIQUE"):
		return q.primaryOrUnique(i, "UNIQUE")
	case q.isIdent(i, "FOREIGN"):
		return q.foreignKey(i)
	case q.isIdent(i, "CHECK"):
		return q.opaqueParenClause(i)
	default:
		return q.columnBody(i)
	}
}

// primaryOrUnique handles "PRIMARY KEY (<cols>) …" or "UNIQUE (<cols>) …",
// quoting the column list and leaving any trailing conflict clause to
// columnBody's generic scan.
func (q *keywordQuoter) primaryOrUnique(i int, lead string) int {
	q.copy(i)
	i++
	i = q.skipWS(i)
	if lead == "PRIMARY" && q.isIdent(i, "KEY") {
		q.copy(i)
		i++
		i = q.skipWS(i)
	}
	if i < q.n && q.els[i].kind == "punct" && q.els[i].text == "(" {
		q.copy(i)
		i++
		i = q.identList(i)
	}
	return q.columnBody(i)
}

// foreignKey handles "FOREIGN KEY (<cols>) REFERENCES <table> (<cols>) …",
// quoting both column lists and the referenced table name.
func (q *keywordQuoter) foreignKey(i int) int {
	q.copy(i)
	i++
	i = q.skipWS(i)
	if q.isIdent(i, "KEY") {
		q.copy(i)
		i++
		i = q.skipWS(i)
	}
	if i < q.n && q.els[i].kind == "punct" && q.els[i].text == "(" {
		q.copy(i)
		i++
		i = q.identList(i)
	}
	i = q.skipWS(i)
	if q.isIdent(i, "REFERENCES") {
		i = q.referencesClause(i)
	}
	return q.columnBody(i)
}

// referencesClause handles "REFERENCES <table> (<cols>)", the target
// position #45 lists for both a table-level FOREIGN KEY and a column-level
// inline REFERENCES.
func (q *keywordQuoter) referencesClause(i int) int {
	q.copy(i)
	i++
	i = q.skipWS(i)
	i = q.quoteIdentAt(i)
	i = q.skipWS(i)
	if i < q.n && q.els[i].kind == "punct" && q.els[i].text == "(" {
		q.copy(i)
		i++
		i = q.identList(i)
	}
	return i
}

// identList quotes the leading column name of each comma-separated entry
// in a parenthesized column list (the '(' was already consumed by the
// caller) — the shape PRIMARY KEY (…), UNIQUE (…), FOREIGN KEY (…) and a
// REFERENCES target all share — while leaving anything else in an entry
// (a COLLATE clause, ASC/DESC) untouched. It consumes the matching closing
// ')' and returns the index past it.
func (q *keywordQuoter) identList(i int) int {
	depth := 1
	entryStart := true
	for i < q.n && depth > 0 {
		i = q.skipWS(i)
		if i >= q.n {
			break
		}
		el := q.els[i]

		if entryStart && depth == 1 && el.kind != "punct" {
			entryStart = false
			i = q.quoteIdentAt(i)
			continue
		}

		switch {
		case el.kind == "punct" && el.text == "(":
			depth++
			q.copy(i)
			i++
		case el.kind == "punct" && el.text == ")":
			depth--
			q.copy(i)
			i++
		case el.kind == "punct" && el.text == "," && depth == 1:
			q.copy(i)
			i++
			entryStart = true
		default:
			q.copy(i)
			i++
		}
	}
	return i
}

// opaqueParenClause copies a parenthesized clause byte-for-byte, tracking
// nested parens only to find the matching close, without quoting anything
// inside — used for CHECK (…), whose expression content is out of #45's
// scope (see quoteCollidingKeywords).
func (q *keywordQuoter) opaqueParenClause(i int) int {
	q.copy(i)
	i++
	i = q.skipWS(i)
	if i >= q.n || q.els[i].kind != "punct" || q.els[i].text != "(" {
		return i
	}
	depth := 1
	q.copy(i)
	i++
	for i < q.n && depth > 0 {
		el := q.els[i]
		if el.kind == "punct" && el.text == "(" {
			depth++
		} else if el.kind == "punct" && el.text == ")" {
			depth--
		}
		q.copy(i)
		i++
	}
	return i
}

// columnBody scans the rest of a column definition (its type and column
// constraints) or the trailing tokens after a table constraint's column
// list, copying everything verbatim except a REFERENCES clause it finds at
// its own depth (folded into referencesClause, the one place #45 requires
// quoting inside a column definition). It stops, without consuming, at the
// comma or closing paren that ends the enclosing entry at that same depth.
func (q *keywordQuoter) columnBody(i int) int {
	depth := 0
	for i < q.n {
		i = q.skipWS(i)
		if i >= q.n {
			break
		}
		el := q.els[i]

		if depth == 0 {
			if el.kind == "punct" && (el.text == "," || el.text == ")") {
				return i
			}
			if el.kind == "ident" && strings.EqualFold(el.text, "REFERENCES") {
				i = q.referencesClause(i)
				continue
			}
		}

		switch {
		case el.kind == "punct" && el.text == "(":
			depth++
			q.copy(i)
			i++
		case el.kind == "punct" && el.text == ")":
			depth--
			q.copy(i)
			i++
		default:
			q.copy(i)
			i++
		}
	}
	return i
}
