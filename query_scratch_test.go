package sitterwasm

import (
	"context"
	"testing"
)

// TestQueryCursorScratchReuseAndGrowth covers the lifetime contract of the
// reusable guest buffer used by the size-probed query iterator ABI. The first
// query needs one capture (the initial 16-byte capacity); a pair query needs
// two captures and therefore exercises geometric growth. Re-executing on the
// same runtime must reuse the original allocation, and closing the cursor must
// return the final allocation to the guest allocator.
func TestQueryCursorScratchReuseAndGrowth(t *testing.T) {
	p, rt, err := NewJSONParser(context.Background())
	if err != nil {
		t.Fatalf("NewJSONParser: %v", err)
	}
	tree, err := p.Parse([]byte(`{"a": 1}`), nil)
	if err != nil {
		_ = p.Close()
		_ = rt.Close()
		t.Fatalf("Parse: %v", err)
	}
	qOne, err := NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		_ = tree.Close()
		_ = p.Close()
		_ = rt.Close()
		t.Fatalf("NewQuery(one capture): %v", err)
	}
	qPair, err := NewQuery(p.Language(), `(pair key: (string) @key value: (_) @value)`)
	if err != nil {
		_ = qOne.Close()
		_ = tree.Close()
		_ = p.Close()
		_ = rt.Close()
		t.Fatalf("NewQuery(two captures): %v", err)
	}
	cursor := NewQueryCursor()
	t.Cleanup(func() {
		_ = cursor.Close()
		_ = qPair.Close()
		_ = qOne.Close()
		_ = tree.Close()
		_ = p.Close()
		_ = rt.Close()
	})

	root := tree.RootNode()
	if err := cursor.Exec(qOne, root); err != nil {
		t.Fatalf("Exec(one capture): %v", err)
	}
	if _, ok := cursor.NextMatch(); !ok {
		t.Fatal("NextMatch(one capture) returned no match")
	}
	firstPtr, firstCap := cursor.queryScratchPtr, cursor.queryScratchCap
	if firstPtr == 0 || firstCap < 16 {
		t.Fatalf("initial scratch = %#x/%d, want non-zero pointer and capacity >= 16", firstPtr, firstCap)
	}
	// Exhaustion releases the tree lifetime gate but intentionally retains the
	// cursor handle and scratch allocation for the next execution.
	if _, ok := cursor.NextMatch(); ok {
		t.Fatal("unexpected extra one-capture match")
	}

	if err := cursor.Exec(qOne, root); err != nil {
		t.Fatalf("re-Exec(one capture): %v", err)
	}
	if _, ok := cursor.NextMatch(); !ok {
		t.Fatal("NextMatch(reused one capture) returned no match")
	}
	if cursor.queryScratchPtr != firstPtr || cursor.queryScratchCap != firstCap {
		t.Fatalf("scratch was not reused: got %#x/%d, want %#x/%d", cursor.queryScratchPtr, cursor.queryScratchCap, firstPtr, firstCap)
	}
	for {
		if _, ok := cursor.NextMatch(); !ok {
			break
		}
	}

	if err := cursor.Exec(qPair, root); err != nil {
		t.Fatalf("Exec(two captures): %v", err)
	}
	pairMatch, ok := cursor.NextMatch()
	if !ok || len(pairMatch.Captures) < 2 {
		t.Fatalf("two-capture query result = %#v, want at least two captures", pairMatch)
	}
	if cursor.queryScratchPtr == 0 || cursor.queryScratchCap <= firstCap {
		t.Fatalf("scratch did not grow for two-capture record: %#x/%d, initial capacity %d", cursor.queryScratchPtr, cursor.queryScratchCap, firstCap)
	}
	grownPtr := cursor.queryScratchPtr
	if _, ok := rt.allocSizes[grownPtr]; !ok {
		t.Fatalf("grown scratch pointer %#x is not tracked by the runtime allocator", grownPtr)
	}

	if err := cursor.Close(); err != nil {
		t.Fatalf("cursor.Close: %v", err)
	}
	if cursor.queryScratchPtr != 0 || cursor.queryScratchCap != 0 {
		t.Fatalf("scratch fields after Close = %#x/%d, want zero", cursor.queryScratchPtr, cursor.queryScratchCap)
	}
	if _, ok := rt.allocSizes[grownPtr]; ok {
		t.Fatalf("scratch pointer %#x remained tracked after cursor Close", grownPtr)
	}
}
