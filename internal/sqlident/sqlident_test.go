package sqlident

import "testing"

func TestScanIdent(t *testing.T) {
	cases := []struct {
		name      string
		s         string
		wantToken string
		wantEnd   int
		wantOK    bool
	}{
		{"unquoted", "user_id INTEGER", "user_id", 7, true},
		{"quoted", `"user id" INTEGER`, `"user id"`, 9, true},
		{"quoted with doubled quote", `"a""b" x`, `"a""b"`, 6, true},
		{"empty", "", "", 0, false},
		{"starts with delimiter", ") x", "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token, end, ok := ScanIdent(c.s, 0)
			if ok != c.wantOK || token != c.wantToken || end != c.wantEnd {
				t.Fatalf("ScanIdent(%q, 0) = (%q, %d, %v), want (%q, %d, %v)",
					c.s, token, end, ok, c.wantToken, c.wantEnd, c.wantOK)
			}
		})
	}
}

// TestIsIdentByte covers issue #29: IsIdentByte must accept '$' and any
// byte >= 0x80, matching SQLite's own unquoted-identifier grammar (verified
// directly: CREATE TABLE foo$bar and CREATE TABLE café are both accepted
// with their name unquoted), not just ASCII letters/digits/underscore.
func TestIsIdentByte(t *testing.T) {
	cases := []struct {
		name string
		c    byte
		want bool
	}{
		{"underscore", '_', true},
		{"ascii letter", 'a', true},
		{"ascii digit", '5', true},
		{"dollar sign", '$', true},
		{"high-bit byte", 0xC3, true},
		{"space", ' ', false},
		{"open paren", '(', false},
		{"double quote", '"', false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsIdentByte(c.c); got != c.want {
				t.Fatalf("IsIdentByte(%q) = %v, want %v", c.c, got, c.want)
			}
		})
	}
}

func TestUnquote(t *testing.T) {
	cases := []struct {
		name  string
		s     string
		quote byte
		want  string
	}{
		{"plain", `"users"`, '"', "users"},
		{"doubled quote inside", `"a""b"`, '"', `a"b`},
		{"not quoted", "users", '"', "users"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Unquote(c.s, c.quote); got != c.want {
				t.Fatalf("Unquote(%q, %q) = %q, want %q", c.s, c.quote, got, c.want)
			}
		})
	}
}
