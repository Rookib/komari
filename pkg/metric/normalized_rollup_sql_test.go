package metric

import (
	"strings"
	"testing"
)

func TestJoinSQLPreservesFragments(t *testing.T) {
	for _, test := range []struct {
		name  string
		parts []string
		want  string
	}{
		{name: "nil"},
		{name: "empty", parts: []string{}},
		{name: "single", parts: []string{"(?)"}, want: "(?)"},
		{name: "empty fragments", parts: []string{"", "?", ""}, want: ", ?, "},
		{name: "numbered parameters", parts: []string{"($1, $2)", "($3, $4)"}, want: "($1, $2), ($3, $4)"},
		{name: "literal fragments", parts: []string{"table.column", "'a,b'", "\"标签\""}, want: "table.column, 'a,b', \"标签\""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := joinSQL(test.parts); got != test.want {
				t.Fatalf("joinSQL(%q) = %q; want %q", test.parts, got, test.want)
			}
		})
	}
	parts := make([]string, 500)
	for i := range parts {
		parts[i] = "(" + strings.Repeat("?, ", 14) + "?)"
	}
	want := strings.Repeat(parts[0]+", ", len(parts)-1) + parts[len(parts)-1]
	if got := joinSQL(parts); got != want {
		t.Fatal("500-row SQL changed")
	}
}

var benchmarkJoinSQLResult string

func BenchmarkJoinSQLRollup500Rows(b *testing.B) {
	parts := make([]string, 500)
	for i := range parts {
		parts[i] = "(" + strings.Repeat("?, ", 14) + "?)"
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkJoinSQLResult = joinSQL(parts)
	}
}
