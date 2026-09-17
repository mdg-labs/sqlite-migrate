// Package rename implements the rename-candidate heuristic: flagging a
// dropped-column-plus-added-column pair (or dropped-table-plus-added-table)
// within the same diff as a likely rename when type and structural
// position are compatible, plus the interactive CLI prompt that confirms
// or declines each candidate.
package rename

// Detect scans a schemadiff.Diff for drop+add pairs whose type and
// structural position make them plausible rename candidates.
