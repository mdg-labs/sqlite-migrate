// Package rename implements the rename-candidate heuristic: flagging a
// dropped-column-plus-added-column pair (or dropped-table-plus-added-table)
// within the same diff as a likely rename when type and structural
// position are compatible, plus the interactive CLI prompt that confirms
// or declines each candidate.
package rename

import (
	"sort"
	"strings"

	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"
)

// Kind distinguishes a table-level rename candidate from a column-level
// one.
type Kind int

const (
	// ColumnRename is a dropped-column-plus-added-column pair within the
	// same (unrenamed) table.
	ColumnRename Kind = iota
	// TableRename is a dropped-table-plus-added-table pair.
	TableRename
)

// Candidate is one rename ambiguity Detect found: a removed name paired
// with an added name that are plausibly the same table or column,
// renamed. Table is empty for a TableRename; for a ColumnRename it is the
// (unchanged) name of the table the column lives in.
type Candidate struct {
	Kind  Kind
	Table string
	From  string
	To    string
}

// Description renders a Candidate the way it should be reported to a
// user, e.g. "orders.customer_name to orders.buyer_name" or
// "table orders to purchases".
func (c Candidate) Description() string {
	if c.Kind == TableRename {
		return "table " + c.From + " to " + c.To
	}
	return c.Table + "." + c.From + " to " + c.Table + "." + c.To
}

// highNameSimilarity is the normalized-Levenshtein-similarity threshold
// above which a name pair counts as a rename signal on its own, even when
// the pair's structural position within the table doesn't line up (e.g. a
// column renamed and also moved past another column being added).
const highNameSimilarity = 0.6

// Detect scans a schemadiff.SchemaDiff for drop+add pairs whose type and
// structural position (or, failing that, a high name-similarity score)
// make them plausible rename candidates. It never decides whether a
// candidate is an actual rename — that is Confirm's job, driven by the
// interactive prompt or the --assume-renames/--assume-no-renames flags.
// The returned candidates are sorted for deterministic output: table
// renames first, then column renames, each ordered by table then from-name.
func Detect(d *schemadiff.SchemaDiff) []Candidate {
	var candidates []Candidate
	candidates = append(candidates, detectTableRenames(d)...)
	for _, td := range d.ChangedTables {
		candidates = append(candidates, detectColumnRenames(td)...)
	}

	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Kind != b.Kind {
			return a.Kind == TableRename
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		return a.From < b.From
	})
	return candidates
}

type scoredPair struct {
	removedIdx int
	addedIdx   int
	score      float64
}

// assignGreedy picks a one-to-one assignment from a set of scored
// candidate pairs, taking the highest-scoring pair first and skipping any
// pair whose removed or added slot has already been claimed. Ties break on
// removed-then-added index for determinism.
func assignGreedy(pairs []scoredPair) []scoredPair {
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].score != pairs[j].score {
			return pairs[i].score > pairs[j].score
		}
		if pairs[i].removedIdx != pairs[j].removedIdx {
			return pairs[i].removedIdx < pairs[j].removedIdx
		}
		return pairs[i].addedIdx < pairs[j].addedIdx
	})

	usedRemoved := make(map[int]bool)
	usedAdded := make(map[int]bool)
	var assigned []scoredPair
	for _, p := range pairs {
		if usedRemoved[p.removedIdx] || usedAdded[p.addedIdx] {
			continue
		}
		usedRemoved[p.removedIdx] = true
		usedAdded[p.addedIdx] = true
		assigned = append(assigned, p)
	}
	return assigned
}

func detectColumnRenames(td schemadiff.TableDiff) []Candidate {
	if len(td.RemovedColumns) == 0 || len(td.AddedColumns) == 0 {
		return nil
	}

	var pairs []scoredPair
	for ri, rc := range td.RemovedColumns {
		for ai, ac := range td.AddedColumns {
			if !strings.EqualFold(rc.Type, ac.Type) {
				continue
			}
			posOK := ri == ai
			sim := nameSimilarity(rc.Name, ac.Name)
			if !posOK && sim < highNameSimilarity {
				continue
			}
			score := sim
			if posOK {
				score += 1
			}
			pairs = append(pairs, scoredPair{removedIdx: ri, addedIdx: ai, score: score})
		}
	}

	var candidates []Candidate
	for _, p := range assignGreedy(pairs) {
		candidates = append(candidates, Candidate{
			Kind:  ColumnRename,
			Table: td.Name,
			From:  td.RemovedColumns[p.removedIdx].Name,
			To:    td.AddedColumns[p.addedIdx].Name,
		})
	}
	return candidates
}

func detectTableRenames(d *schemadiff.SchemaDiff) []Candidate {
	if len(d.RemovedTables) == 0 || len(d.AddedTables) == 0 {
		return nil
	}

	var pairs []scoredPair
	for ri, rt := range d.RemovedTables {
		for ai, at := range d.AddedTables {
			structOK := columnSignaturesMatch(rt.Columns, at.Columns)
			sim := nameSimilarity(rt.Name, at.Name)
			if !structOK && sim < highNameSimilarity {
				continue
			}
			score := sim
			if structOK {
				score += 1
			}
			pairs = append(pairs, scoredPair{removedIdx: ri, addedIdx: ai, score: score})
		}
	}

	var candidates []Candidate
	for _, p := range assignGreedy(pairs) {
		candidates = append(candidates, Candidate{
			Kind: TableRename,
			From: d.RemovedTables[p.removedIdx].Name,
			To:   d.AddedTables[p.addedIdx].Name,
		})
	}
	return candidates
}

// columnSignaturesMatch reports whether two tables have the same number of
// columns with the same types in the same order, the "structural position"
// signal for a whole-table rename candidate — column names are deliberately
// ignored, since those are exactly what a rename changes.
func columnSignaturesMatch(a, b []schemadiff.Column) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i].Type, b[i].Type) {
			return false
		}
	}
	return true
}

// nameSimilarity returns a normalized similarity score in [0, 1] between
// two identifiers, based on case-insensitive Levenshtein edit distance:
// 1 means identical, 0 means completely dissimilar.
func nameSimilarity(a, b string) float64 {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	maxLen := len(ra)
	if len(rb) > maxLen {
		maxLen = len(rb)
	}
	if maxLen == 0 {
		return 1
	}
	return 1 - float64(levenshtein(ra, rb))/float64(maxLen)
}

// levenshtein computes the classic single-character insert/delete/replace
// edit distance between two rune slices.
func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			min := del
			if ins < min {
				min = ins
			}
			if sub < min {
				min = sub
			}
			curr[j] = min
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
