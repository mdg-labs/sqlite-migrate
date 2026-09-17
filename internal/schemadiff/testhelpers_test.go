package schemadiff

import (
	"context"
	"testing"
)

func mustParse(t *testing.T, ddl string) *Schema {
	t.Helper()
	s, err := Parse(context.Background(), ddl)
	if err != nil {
		t.Fatalf("Parse failed: %v\nDDL:\n%s", err, ddl)
	}
	return s
}

func diffColumnNames(cols []Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}
