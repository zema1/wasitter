package sitterwasm_test

import (
	"testing"
	"time"

	sitterwasm "github.com/zema1/sitterwasm"
)

// A progress callback cancels host-side collection after a result has been
// observed.  The cursor must destroy its native handle before releasing the
// tree lifetime gate; otherwise Tree.Close can race a still-live native cursor
// that retains pointers into the tree.
func TestQueryCursorProgressCancellationReleasesNativeHandleBeforeTree(t *testing.T) {
	p, _, tree := parseJSON(t, `[1, 2, 3, 4]`)
	q, err := sitterwasm.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	defer q.Close()
	cursor := sitterwasm.NewQueryCursor()
	defer cursor.Close()

	callbacks := 0
	matches := cursor.MatchesWithOptions(
		q,
		tree.RootNode(),
		[]byte(`[1, 2, 3, 4]`),
		sitterwasm.QueryCursorOptions{ProgressCallback: func(sitterwasm.QueryCursorState) bool {
			callbacks++
			return true
		}},
	)
	if callbacks == 0 {
		t.Fatal("progress callback was not invoked")
	}
	if len(matches) == 0 {
		t.Fatal("canceled query did not return the first materialized match")
	}
	if got := cursor.Handle(); got != 0 {
		t.Fatalf("cursor handle after cancellation = %d, want 0", got)
	}

	closed := make(chan error, 1)
	go func() { closed <- tree.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Tree.Close after progress cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Tree.Close remained blocked after progress cancellation")
	}
}

// A native TSQueryCursor retains pointers into the execution tree after Exec
// returns. The cursor must therefore keep the tree alive until the iterator is
// exhausted (or explicitly closed); otherwise a concurrent Tree.Close can
// free the guest TSTree before the next cursor call.
func TestQueryCursorRetainsTreeUntilStreamExhausted(t *testing.T) {
	p, rt, tree := parseJSON(t, `[1, 2, 3]`)
	q, err := sitterwasm.NewQuery(p.Language(), `(number) @n`)
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	cursor := sitterwasm.NewQueryCursor()
	defer cursor.Close()
	defer q.Close()

	if err := cursor.Exec(q, tree.RootNode()); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if _, ok := cursor.NextMatch(); !ok {
		t.Fatal("NextMatch returned no first match")
	}

	closed := make(chan error, 1)
	go func() { closed <- tree.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Tree.Close completed before query stream was exhausted: %v", err)
	case <-time.After(25 * time.Millisecond):
		// Expected: QueryCursor owns the tree lifetime gate while results remain.
	}

	// Drain the remaining native stream. The terminal NextMatch call releases
	// the retained lifetime gate and allows the pending close to complete.
	for {
		if _, ok := cursor.NextMatch(); !ok {
			break
		}
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Tree.Close after stream exhaustion: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Tree.Close remained blocked after query stream exhaustion")
	}
	_ = rt // parseJSON owns and closes the runtime in its test cleanup.
}
