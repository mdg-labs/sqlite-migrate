package rename

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

var testCandidate = Candidate{Kind: ColumnRename, Table: "orders", From: "customer_name", To: "buyer_name"}

func TestConfirm_InteractivePrompt_Confirmed(t *testing.T) {
	var out bytes.Buffer
	got, err := Confirm(context.Background(), bufio.NewReader(strings.NewReader("y\n")), &out, testCandidate, Flags{})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !got {
		t.Fatalf("Confirm: got false, want true")
	}
	if !strings.Contains(out.String(), "Rename orders.customer_name to orders.buyer_name? [Y/n]") {
		t.Errorf("prompt text: got %q", out.String())
	}
}

func TestConfirm_InteractivePrompt_YesVariants(t *testing.T) {
	for _, answer := range []string{"y\n", "Y\n", "yes\n", "YES\n", "  y  \n"} {
		got, err := Confirm(context.Background(), bufio.NewReader(strings.NewReader(answer)), &bytes.Buffer{}, testCandidate, Flags{})
		if err != nil {
			t.Fatalf("Confirm(%q): %v", answer, err)
		}
		if !got {
			t.Errorf("Confirm(%q): got false, want true", answer)
		}
	}
}

func TestConfirm_InteractivePrompt_Declined(t *testing.T) {
	for _, answer := range []string{"n\n", "N\n", "no\n", "NO\n"} {
		got, err := Confirm(context.Background(), bufio.NewReader(strings.NewReader(answer)), &bytes.Buffer{}, testCandidate, Flags{})
		if err != nil {
			t.Fatalf("Confirm(%q): %v", answer, err)
		}
		if got {
			t.Errorf("Confirm(%q): got true, want false", answer)
		}
	}
}

// Pressing Enter with no other input accepts the capitalized "Y" default
// shown in the prompt.
func TestConfirm_InteractivePrompt_EmptyAnswerDefaultsYes(t *testing.T) {
	got, err := Confirm(context.Background(), bufio.NewReader(strings.NewReader("\n")), &bytes.Buffer{}, testCandidate, Flags{})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !got {
		t.Fatalf("Confirm: got false, want true (empty answer should default to yes)")
	}
}

// Invalid input is reprompted rather than accepted or silently declined.
func TestConfirm_InteractivePrompt_InvalidInputReprompts(t *testing.T) {
	var out bytes.Buffer
	got, err := Confirm(context.Background(), bufio.NewReader(strings.NewReader("maybe\nn\n")), &out, testCandidate, Flags{})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if got {
		t.Fatalf("Confirm: got true, want false")
	}
	if strings.Count(out.String(), "Rename ") != 2 {
		t.Errorf("want the prompt reissued after invalid input, got:\n%s", out.String())
	}
}

// Running non-interactively with nothing on stdin at all (no terminal
// attached, no assume-* flag set) falls back to declining, not to the
// prompt's own "Y" default — this is what keeps an unattended CI run from
// silently treating an ambiguous drop+add as a confirmed rename.
func TestConfirm_NoInputAtAll_FallsBackToDeclined(t *testing.T) {
	got, err := Confirm(context.Background(), bufio.NewReader(strings.NewReader("")), &bytes.Buffer{}, testCandidate, Flags{})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if got {
		t.Fatalf("Confirm: got true, want false (unattended run must not silently confirm)")
	}
}

// An answer present on the reader without a trailing newline (the stream
// ends right after it) is still read correctly.
func TestConfirm_AnswerWithoutTrailingNewline(t *testing.T) {
	got, err := Confirm(context.Background(), bufio.NewReader(strings.NewReader("n")), &bytes.Buffer{}, testCandidate, Flags{})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if got {
		t.Fatalf("Confirm: got true, want false")
	}
}

func TestConfirm_AssumeRenames_SkipsPrompt(t *testing.T) {
	var out bytes.Buffer
	got, err := Confirm(context.Background(), bufio.NewReader(strings.NewReader("")), &out, testCandidate, Flags{AssumeRenames: true})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !got {
		t.Fatalf("Confirm: got false, want true")
	}
	if out.Len() != 0 {
		t.Errorf("want no prompt printed with --assume-renames, got %q", out.String())
	}
}

func TestConfirm_AssumeNoRenames_SkipsPrompt(t *testing.T) {
	var out bytes.Buffer
	got, err := Confirm(context.Background(), bufio.NewReader(strings.NewReader("")), &out, testCandidate, Flags{AssumeNoRenames: true})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if got {
		t.Fatalf("Confirm: got true, want false")
	}
	if out.Len() != 0 {
		t.Errorf("want no prompt printed with --assume-no-renames, got %q", out.String())
	}
}

func TestConfirm_ConflictingAssumeFlags_Errors(t *testing.T) {
	_, err := Confirm(context.Background(), bufio.NewReader(strings.NewReader("")), &bytes.Buffer{}, testCandidate, Flags{AssumeRenames: true, AssumeNoRenames: true})
	if !errors.Is(err, ErrConflictingAssumeFlags) {
		t.Fatalf("want ErrConflictingAssumeFlags, got %v", err)
	}
}

// Resolve exercises the full pipeline end to end: a genuine rename
// confirmed via scripted input, and an unrelated drop+add sharing a type
// (the false-positive case Detect can't rule out on its own) declined via
// scripted input and correctly falling back to "not a rename" rather than
// being silently treated as one.
func TestResolve_ScriptedInput_TrueRenameAndFalsePositive(t *testing.T) {
	d := diffOf(t, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			customer_name TEXT NOT NULL
		) STRICT;
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			ssn TEXT
		) STRICT;
	`, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			buyer_name TEXT NOT NULL
		) STRICT;
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			last_login TEXT
		) STRICT;
	`)

	// Detect finds the orders and users candidates sorted by table name:
	// "orders" before "users" — answer the first "y" (confirm the real
	// rename), the second "n" (decline the false positive).
	var out bytes.Buffer
	resolutions, err := Resolve(context.Background(), d, strings.NewReader("y\nn\n"), &out, Flags{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolutions) != 2 {
		t.Fatalf("want 2 resolutions, got %d: %+v", len(resolutions), resolutions)
	}

	ordersRes := resolutions[0]
	if ordersRes.Candidate.Table != "orders" || !ordersRes.Confirmed {
		t.Errorf("want orders.customer_name->buyer_name confirmed, got %+v", ordersRes)
	}
	usersRes := resolutions[1]
	if usersRes.Candidate.Table != "users" || usersRes.Confirmed {
		t.Errorf("want users.ssn->last_login declined, got %+v", usersRes)
	}
}

// Confirm must share one bufio.Reader across every candidate in a Resolve
// run: re-wrapping the reader per call would let the first call's bufio
// buffer swallow bytes meant for the next candidate's answer, so a later
// "y" would be silently lost and default to declined.
func TestResolve_ScriptedInput_SharesReaderAcrossCandidates(t *testing.T) {
	d := diffOf(t, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			customer_name TEXT NOT NULL
		) STRICT;
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			ssn TEXT
		) STRICT;
	`, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			buyer_name TEXT NOT NULL
		) STRICT;
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			last_login TEXT
		) STRICT;
	`)

	// Detect orders candidates by table name: "orders" before "users".
	// Reverse the answers from TestResolve_ScriptedInput_TrueRenameAndFalsePositive
	// ("n" then "y") so a reader that gets reset between calls can't
	// coincidentally pass by defaulting both answers to declined.
	resolutions, err := Resolve(context.Background(), d, strings.NewReader("n\ny\n"), &bytes.Buffer{}, Flags{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolutions) != 2 {
		t.Fatalf("want 2 resolutions, got %d: %+v", len(resolutions), resolutions)
	}

	ordersRes := resolutions[0]
	if ordersRes.Candidate.Table != "orders" || ordersRes.Confirmed {
		t.Errorf("want orders.customer_name->buyer_name declined, got %+v", ordersRes)
	}
	usersRes := resolutions[1]
	if usersRes.Candidate.Table != "users" || !usersRes.Confirmed {
		t.Errorf("want users.ssn->last_login confirmed, got %+v", usersRes)
	}
}

// The same false-positive-heavy diff run with --assume-no-renames never
// prompts at all and declines every candidate, the CI-friendly path.
func TestResolve_AssumeNoRenames_DeclinesEverythingWithoutPrompting(t *testing.T) {
	d := diffOf(t, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			customer_name TEXT NOT NULL
		) STRICT;
	`, `
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			buyer_name TEXT NOT NULL
		) STRICT;
	`)

	var out bytes.Buffer
	resolutions, err := Resolve(context.Background(), d, strings.NewReader(""), &out, Flags{AssumeNoRenames: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolutions) != 1 || resolutions[0].Confirmed {
		t.Fatalf("want 1 declined resolution, got %+v", resolutions)
	}
	if out.Len() != 0 {
		t.Errorf("want no prompt printed, got %q", out.String())
	}
}
