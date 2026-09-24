package sqldefwrap

import (
	"fmt"
	"strings"
	"sync"

	"github.com/sqldef/sqldef/v3/database"
	"github.com/sqldef/sqldef/v3/parser"
)

// exprMasker replaces each CREATE TABLE CHECK/DEFAULT/GENERATED expression
// body sqldef's own SQLite parser can't parse (SQLite operators outside
// its shared multi-dialect grammar, e.g. GLOB, MATCH, IS <expr>, or a bare
// identifier colliding with a sqldef keyword) with an opaque placeholder
// identifier before handing the statement to sqldef, and restores the
// original body text in sqldef's output afterwards. A body sqldef already
// accepts passes through unmasked, so its normal output is unchanged.
// Placeholders are keyed by exact body text, so the same expression on
// both sides of a Diff call gets the same placeholder and sqldef sees no
// change there.
type exprMasker struct {
	nonce        string
	next         int
	placeholders map[string]string // body text -> placeholder identifier
	bodies       map[string]string // placeholder identifier -> body text
}

// newExprMasker picks a placeholder prefix guaranteed absent from every
// input string, so a minted placeholder can never collide with an
// identifier already present there.
func newExprMasker(inputs ...string) *exprMasker {
	nonce := "sqlite_migrate_expr"
	for {
		collides := false
		for _, in := range inputs {
			if strings.Contains(in, nonce) {
				collides = true
				break
			}
		}
		if !collides {
			break
		}
		nonce += "_x"
	}
	return &exprMasker{
		nonce:        nonce,
		placeholders: make(map[string]string),
		bodies:       make(map[string]string),
	}
}

func (m *exprMasker) placeholderFor(body string) string {
	if ph, ok := m.placeholders[body]; ok {
		return ph
	}
	m.next++
	ph := fmt.Sprintf("%s_%d", m.nonce, m.next)
	m.placeholders[body] = ph
	m.bodies[ph] = body
	return ph
}

// mask rewrites every CREATE TABLE statement in sql, replacing a rejected
// CHECK/DEFAULT/GENERATED expression body with its placeholder. Anything
// outside a CREATE TABLE statement (a CREATE INDEX, a comment, …) and
// everything in a CREATE TABLE besides those three expression positions
// is copied through unchanged.
func (m *exprMasker) mask(sql string) string {
	w := &exprMaskWalker{m: m, els: tokenizeForKeywordQuoting(sql)}
	w.n = len(w.els)
	i := 0
	for i < w.n {
		if w.isIdent(i, "CREATE") {
			w.copy(i)
			i++
			i = w.afterCreate(i)
			continue
		}
		w.copy(i)
		i++
	}
	return w.out.String()
}

// unmask restores every placeholder this masker minted to its original
// expression body text in each of ddls. It returns an error instead of a
// statement if this masker's nonce appears anywhere else — an unknown
// bare identifier, or inside a quoted string/identifier or a comment.
// newExprMasker guarantees the nonce is absent from every input, so any
// such occurrence means a placeholder reached sqldef's output somewhere
// this masker didn't expect to find or restore it, which must never be
// allowed to reach a migration file unexplained.
func (m *exprMasker) unmask(ddls []string) ([]string, error) {
	out := make([]string, len(ddls))
	for i, ddl := range ddls {
		restored, err := m.unmaskOne(ddl)
		if err != nil {
			return nil, err
		}
		out[i] = restored
	}
	return out, nil
}

func (m *exprMasker) unmaskOne(ddl string) (string, error) {
	els := tokenizeForKeywordQuoting(ddl)
	var b strings.Builder
	for _, el := range els {
		if el.kind == "ident" {
			if body, ok := m.bodies[el.text]; ok {
				b.WriteString(body)
				continue
			}
		}
		if strings.Contains(el.text, m.nonce) {
			return "", fmt.Errorf("sqldefwrap: unresolved expression placeholder %q in generated DDL %q", el.text, ddl)
		}
		b.WriteString(el.text)
	}
	return b.String(), nil
}

// exprRejectCache memoizes, per exact (clause kind, body text) pair,
// whether sqldef's own SQLite parser rejects that expression body in that
// clause position — the same probe-don't-list technique
// sqldefRejectsIdentifier uses for colliding keywords.
var exprRejectCache sync.Map // string -> bool

func sqldefRejectsExpression(kind, body string) bool {
	key := kind + "\x00" + body
	if v, ok := exprRejectCache.Load(key); ok {
		return v.(bool)
	}
	var probe string
	switch kind {
	case "default":
		probe = fmt.Sprintf("CREATE TABLE _sqlite_migrate_probe_ (a INTEGER DEFAULT (%s))", body)
	case "generated":
		probe = fmt.Sprintf("CREATE TABLE _sqlite_migrate_probe_ (a INTEGER, b INTEGER GENERATED ALWAYS AS (%s) VIRTUAL)", body)
	default: // "check"
		probe = fmt.Sprintf("CREATE TABLE _sqlite_migrate_probe_ (a INTEGER CHECK (%s))", body)
	}
	_, err := database.NewParser(parser.ParserModeSQLite3).Parse(probe)
	rejected := err != nil
	exprRejectCache.Store(key, rejected)
	return rejected
}

// exprMaskWalker walks the token stream produced by
// tokenizeForKeywordQuoting (shared with quoteCollidingKeywords), copying
// it to out unchanged except at a CHECK/DEFAULT/GENERATED expression
// body, which is replaced by its placeholder when rejected.
type exprMaskWalker struct {
	m   *exprMasker
	els []qkElement
	out strings.Builder
	n   int
}

func (w *exprMaskWalker) copy(i int) { w.out.WriteString(w.els[i].text) }

func (w *exprMaskWalker) isIdent(i int, word string) bool {
	return i < w.n && w.els[i].kind == "ident" && strings.EqualFold(w.els[i].text, word)
}

func (w *exprMaskWalker) skipWS(i int) int {
	for i < w.n && (w.els[i].kind == "ws" || w.els[i].kind == "comment") {
		w.copy(i)
		i++
	}
	return i
}

// afterCreate dispatches to createTable for a CREATE TABLE statement.
// Anything else CREATE can introduce (INDEX, TRIGGER, VIEW) carries no
// CREATE TABLE CHECK/DEFAULT/GENERATED expression this masker handles, so
// it is left for the caller's own token loop to copy through unexamined.
func (w *exprMaskWalker) afterCreate(i int) int {
	j := w.skipWS(i)
	if !w.isIdent(j, "TABLE") {
		return i
	}
	i = j
	w.copy(i)
	i++
	return w.createTable(i)
}

// ifNotExists copies an optional "IF NOT EXISTS" and returns the index of
// the significant token after it.
func (w *exprMaskWalker) ifNotExists(i int) int {
	i = w.skipWS(i)
	if w.isIdent(i, "IF") {
		w.copy(i)
		i++
		i = w.skipWS(i)
		if w.isIdent(i, "NOT") {
			w.copy(i)
			i++
			i = w.skipWS(i)
		}
		if w.isIdent(i, "EXISTS") {
			w.copy(i)
			i++
			i = w.skipWS(i)
		}
	}
	return i
}

// createTable handles "[IF NOT EXISTS] <table-name> ( <column-list> )",
// copying the table name verbatim and delegating the column list to
// columnList. Anything after the closing paren (STRICT, WITHOUT ROWID,
// the trailing ';') is left for the caller's own token loop.
func (w *exprMaskWalker) createTable(i int) int {
	i = w.ifNotExists(i)
	if i < w.n {
		w.copy(i)
		i++
	}
	i = w.skipWS(i)
	if i < w.n && w.els[i].kind == "punct" && w.els[i].text == "(" {
		w.copy(i)
		i++
		i = w.columnList(i)
	}
	return i
}

// columnList walks the comma-separated body of a CREATE TABLE at depth 1
// (the '(' that opens it was already consumed by the caller), dispatching
// each entry to tableConstraint or a column definition, until the matching
// closing ')' returns depth to 0.
func (w *exprMaskWalker) columnList(i int) int {
	depth := 1
	defStart := true
	for i < w.n && depth > 0 {
		i = w.skipWS(i)
		if i >= w.n {
			break
		}
		el := w.els[i]

		if defStart {
			defStart = false
			if el.kind == "ident" && isTableConstraintLead(el.text) {
				i = w.tableConstraint(i)
				continue
			}
			w.copy(i) // column name
			i++
			i = w.columnBody(i)
			continue
		}

		switch {
		case el.kind == "punct" && el.text == "(":
			depth++
			w.copy(i)
			i++
		case el.kind == "punct" && el.text == ")":
			depth--
			w.copy(i)
			i++
		case el.kind == "punct" && el.text == "," && depth == 1:
			w.copy(i)
			i++
			defStart = true
		default:
			w.copy(i)
			i++
		}
	}
	return i
}

// tableConstraint dispatches a table-level constraint clause (the current
// token is one of CONSTRAINT/PRIMARY/UNIQUE/CHECK/FOREIGN, already
// confirmed by isTableConstraintLead) to the handler for its kind.
func (w *exprMaskWalker) tableConstraint(i int) int {
	if w.isIdent(i, "CONSTRAINT") {
		w.copy(i)
		i++
		i = w.skipWS(i)
		if i < w.n {
			w.copy(i) // constraint name
			i++
		}
		i = w.skipWS(i)
		return w.tableConstraint(i)
	}
	switch {
	case w.isIdent(i, "PRIMARY"):
		return w.primaryOrUnique(i, "PRIMARY")
	case w.isIdent(i, "UNIQUE"):
		return w.primaryOrUnique(i, "UNIQUE")
	case w.isIdent(i, "FOREIGN"):
		return w.foreignKey(i)
	case w.isIdent(i, "CHECK"):
		return w.maskParenClause(i, "check")
	default:
		return w.columnBody(i)
	}
}

// primaryOrUnique handles "PRIMARY KEY (<cols>) …" or "UNIQUE (<cols>) …".
// SQLite's grammar never allows an arbitrary expression inside a table-
// level PRIMARY KEY/UNIQUE column list, so it is copied through verbatim;
// any trailing conflict clause is left to columnBody's generic scan.
func (w *exprMaskWalker) primaryOrUnique(i int, lead string) int {
	w.copy(i)
	i++
	i = w.skipWS(i)
	if lead == "PRIMARY" && w.isIdent(i, "KEY") {
		w.copy(i)
		i++
		i = w.skipWS(i)
	}
	if i < w.n && w.els[i].kind == "punct" && w.els[i].text == "(" {
		i = w.copyParenList(i)
	}
	return w.columnBody(i)
}

// foreignKey handles "FOREIGN KEY (<cols>) REFERENCES <table> (<cols>) …",
// copying both column lists and the referenced table verbatim — none of
// which SQLite's grammar allows an arbitrary expression inside.
func (w *exprMaskWalker) foreignKey(i int) int {
	w.copy(i)
	i++
	i = w.skipWS(i)
	if w.isIdent(i, "KEY") {
		w.copy(i)
		i++
		i = w.skipWS(i)
	}
	if i < w.n && w.els[i].kind == "punct" && w.els[i].text == "(" {
		i = w.copyParenList(i)
	}
	i = w.skipWS(i)
	if w.isIdent(i, "REFERENCES") {
		w.copy(i)
		i++
		i = w.skipWS(i)
		if i < w.n {
			w.copy(i) // referenced table name
			i++
		}
		i = w.skipWS(i)
		if i < w.n && w.els[i].kind == "punct" && w.els[i].text == "(" {
			i = w.copyParenList(i)
		}
	}
	return w.columnBody(i)
}

// copyParenList copies a parenthesized list verbatim (the '(' at i is
// copied too), tracking nested parens only to find the matching close.
// Used for PRIMARY KEY/UNIQUE/FOREIGN KEY column lists and REFERENCES
// targets.
func (w *exprMaskWalker) copyParenList(i int) int {
	w.copy(i)
	i++
	depth := 1
	for i < w.n && depth > 0 {
		el := w.els[i]
		if el.kind == "punct" && el.text == "(" {
			depth++
		} else if el.kind == "punct" && el.text == ")" {
			depth--
		}
		w.copy(i)
		i++
	}
	return i
}

// columnBody scans the rest of a column definition (its type and column
// constraints) or the trailing tokens after a table constraint's column
// list, copying everything verbatim except a CHECK (…), DEFAULT (…) or
// [GENERATED ALWAYS] AS (…) expression body found at its own depth, which
// is handed to maskParenClause. It stops, without consuming, at the comma
// or closing paren that ends the enclosing entry at that same depth.
func (w *exprMaskWalker) columnBody(i int) int {
	depth := 0
	for i < w.n {
		i = w.skipWS(i)
		if i >= w.n {
			break
		}
		el := w.els[i]

		if depth == 0 && el.kind == "punct" && (el.text == "," || el.text == ")") {
			return i
		}
		if depth == 0 && el.kind == "ident" {
			switch {
			case strings.EqualFold(el.text, "CHECK"):
				i = w.maskParenClause(i, "check")
				continue
			case strings.EqualFold(el.text, "DEFAULT"):
				if w.parenFollows(i) {
					i = w.maskParenClause(i, "default")
					continue
				}
			case strings.EqualFold(el.text, "AS"):
				if w.parenFollows(i) {
					i = w.maskParenClause(i, "generated")
					continue
				}
			}
		}

		switch {
		case el.kind == "punct" && el.text == "(":
			depth++
			w.copy(i)
			i++
		case el.kind == "punct" && el.text == ")":
			depth--
			w.copy(i)
			i++
		default:
			w.copy(i)
			i++
		}
	}
	return i
}

// parenFollows reports whether the next significant token after els[i]
// (skipping only whitespace/comments, without consuming anything) is "(".
func (w *exprMaskWalker) parenFollows(i int) bool {
	j := i + 1
	for j < w.n && (w.els[j].kind == "ws" || w.els[j].kind == "comment") {
		j++
	}
	return j < w.n && w.els[j].kind == "punct" && w.els[j].text == "("
}

// maskParenClause copies els[i] (the CHECK/DEFAULT/AS keyword already
// confirmed by the caller) followed by its parenthesized expression body,
// replacing the body with its placeholder when sqldefRejectsExpression
// rejects it for kind, and returns the index past the closing ')'.
func (w *exprMaskWalker) maskParenClause(i int, kind string) int {
	w.copy(i)
	i++
	i = w.skipWS(i)
	if i >= w.n || w.els[i].kind != "punct" || w.els[i].text != "(" {
		return i
	}
	w.copy(i)
	i++

	depth := 1
	start := i
	for i < w.n && depth > 0 {
		el := w.els[i]
		if el.kind == "punct" && el.text == "(" {
			depth++
		} else if el.kind == "punct" && el.text == ")" {
			depth--
			if depth == 0 {
				break
			}
		}
		i++
	}
	var body strings.Builder
	for _, el := range w.els[start:i] {
		body.WriteString(el.text)
	}

	if sqldefRejectsExpression(kind, body.String()) {
		w.out.WriteString(w.m.placeholderFor(body.String()))
	} else {
		w.out.WriteString(body.String())
	}

	if i < w.n {
		w.copy(i) // ")"
		i++
	}
	return i
}
