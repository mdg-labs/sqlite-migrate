package sqlitemigrate

import "testing"

func TestChecksum_Deterministic(t *testing.T) {
	sql := "CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;"
	first := Checksum(sql)
	second := Checksum(sql)
	if first != second {
		t.Fatalf("Checksum is not deterministic for the same input: %q != %q", first, second)
	}
}

func TestChecksum_DiffersOnContentChange(t *testing.T) {
	a := "CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;"
	b := "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT) STRICT;"
	if Checksum(a) == Checksum(b) {
		t.Fatal("Checksum did not change when the SQL body changed")
	}
}

func TestVerifyChecksum(t *testing.T) {
	sql := "CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT;"
	sum := Checksum(sql)

	if !VerifyChecksum(sql, sum) {
		t.Fatal("VerifyChecksum rejected a matching checksum")
	}
	if VerifyChecksum(sql+" -- tampered", sum) {
		t.Fatal("VerifyChecksum accepted a tampered migration body")
	}
}
