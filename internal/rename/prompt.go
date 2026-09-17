package rename

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mdg-labs/sqlite-migrate/internal/schemadiff"
)

// Flags controls how Confirm resolves a rename Candidate without reading
// from the terminal, for CI or any other non-interactive run.
// AssumeRenames and AssumeNoRenames are mutually exclusive; leaving both
// false means "ask interactively".
type Flags struct {
	AssumeRenames   bool
	AssumeNoRenames bool
}

// ErrConflictingAssumeFlags reports that both --assume-renames and
// --assume-no-renames were set, which is a caller error, not an ambiguous
// diff.
var ErrConflictingAssumeFlags = errors.New("rename: --assume-renames and --assume-no-renames are mutually exclusive")

// Confirm resolves one rename Candidate: true means treat it as a rename
// (generate RENAME COLUMN/RENAME TABLE, preserving data); false means fall
// back to treating it as a genuine drop-and-add, gated behind
// --allow-destructive.
//
// When flags sets an assume-* option, Confirm returns immediately without
// touching in or out. Otherwise it prints the candidate as a
// "Rename X to Y? [Y/n]" prompt to out and reads an answer line from in.
// An empty answer (the user just pressing Enter) confirms, matching the
// capitalized "Y" default shown in the prompt. Declining ("n"/"no"), or in
// is exhausted before any answer is read at all (no terminal attached,
// non-interactive with no assume-* flag set), falls back to declining.
func Confirm(in io.Reader, out io.Writer, c Candidate, flags Flags) (bool, error) {
	if flags.AssumeRenames && flags.AssumeNoRenames {
		return false, ErrConflictingAssumeFlags
	}
	if flags.AssumeRenames {
		return true, nil
	}
	if flags.AssumeNoRenames {
		return false, nil
	}

	reader := bufio.NewReader(in)
	for {
		if _, err := fmt.Fprintf(out, "Rename %s? [Y/n] ", c.Description()); err != nil {
			return false, err
		}

		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return false, nil
		}

		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}

		if err != nil {
			return false, nil
		}
		if _, err := fmt.Fprintln(out, `please answer "y" or "n"`); err != nil {
			return false, err
		}
	}
}

// Resolution pairs a Candidate Detect found with the decision Confirm made
// about it.
type Resolution struct {
	Candidate Candidate
	Confirmed bool
}

// Resolve runs Detect against d, then Confirm for every candidate it finds,
// in Detect's deterministic order, returning each candidate paired with its
// decision.
func Resolve(d *schemadiff.SchemaDiff, in io.Reader, out io.Writer, flags Flags) ([]Resolution, error) {
	candidates := Detect(d)
	resolutions := make([]Resolution, 0, len(candidates))
	for _, c := range candidates {
		confirmed, err := Confirm(in, out, c, flags)
		if err != nil {
			return nil, err
		}
		resolutions = append(resolutions, Resolution{Candidate: c, Confirmed: confirmed})
	}
	return resolutions, nil
}
