package schemadiff

import "sort"

// Verdict is the result of classifying a Diff.
type Verdict int

const (
	// Safe means every table and column present before the change is
	// still present after it, regardless of type or constraint changes —
	// the change can be auto-generated (including a full rebuild) without
	// asking for confirmation.
	Safe Verdict = iota
	// Destructive means at least one table or column present before the
	// change is genuinely absent after it — generating the change
	// requires the destructive-change gate.
	Destructive
)

func (v Verdict) String() string {
	if v == Destructive {
		return "destructive"
	}
	return "safe"
}

// Classification is the outcome of Classify: the verdict, plus exactly
// which tables/columns would be lost when the verdict is Destructive, so a
// caller can report precisely what's at risk without re-deriving it from
// the SQL.
type Classification struct {
	Verdict Verdict

	// RemovedTables lists every table dropped entirely, sorted by name.
	RemovedTables []string

	// RemovedColumns maps each changed table's name to the columns
	// removed from it. A table listed in RemovedTables never also
	// appears here — its columns aren't "removed", the whole table is.
	RemovedColumns map[string][]string
}

// Classify decides whether a Diff is safe or destructive by comparing
// table and column presence between the two structured schemas, never by
// scanning generated SQL text for statements like DROP. Every rebuild —
// even one that discards no data — involves creating a new table and
// dropping the old one internally, so any text-based check on generated
// SQL would misclassify safe changes as destructive.
func Classify(d *SchemaDiff) Classification {
	c := Classification{Verdict: Safe}

	if len(d.RemovedTables) > 0 {
		c.Verdict = Destructive
		c.RemovedTables = make([]string, 0, len(d.RemovedTables))
		for _, t := range d.RemovedTables {
			c.RemovedTables = append(c.RemovedTables, t.Name)
		}
		sort.Strings(c.RemovedTables)
	}

	for _, td := range d.ChangedTables {
		if len(td.RemovedColumns) == 0 {
			continue
		}
		c.Verdict = Destructive
		if c.RemovedColumns == nil {
			c.RemovedColumns = make(map[string][]string)
		}
		names := make([]string, 0, len(td.RemovedColumns))
		for _, col := range td.RemovedColumns {
			names = append(names, col.Name)
		}
		sort.Strings(names)
		c.RemovedColumns[td.Name] = names
	}

	return c
}
