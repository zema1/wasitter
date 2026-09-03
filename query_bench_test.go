package sitterwasm_test

import (
	"context"
	"testing"

	sitterwasm "github.com/zema1/sitterwasm"
)

func BenchmarkNativeQueryMatches(b *testing.B) {
	p, rt, err := sitterwasm.NewJSONParser(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()
	tree, err := p.Parse(benchmarkJSON, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tree.Close()
	query, err := sitterwasm.NewQuery(p.Language(), `(number) @number`)
	if err != nil {
		b.Fatal(err)
	}
	defer query.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		matches := query.Matches(tree.RootNode())
		if len(matches) == 0 {
			b.Fatal("query returned no matches")
		}
	}
}

func BenchmarkNativeQueryCursor(b *testing.B) {
	p, rt, err := sitterwasm.NewJSONParser(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	defer rt.Close()
	defer p.Close()
	tree, err := p.Parse(benchmarkJSON, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tree.Close()
	query, err := sitterwasm.NewQuery(p.Language(), `(string) @string`)
	if err != nil {
		b.Fatal(err)
	}
	defer query.Close()
	cursor := sitterwasm.NewQueryCursor()
	defer cursor.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cursor.Exec(query, tree.RootNode()); err != nil {
			b.Fatal(err)
		}
		for {
			if _, ok := cursor.NextMatch(); !ok {
				break
			}
		}
	}
}
