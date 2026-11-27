package server

import (
	"sync"
	"testing"
)

// TestToolCallIndexerUniquePerTurn guards the regression that broke every parallel tool call.
//
// @purpose Proves per-turn tool-call indexes are distinct and gap-free.
// @rationale OpenAI clients fold streamed deltas sharing an index into ONE call and concatenate the
// argument fragments; the streaming handler previously sent index 0 for every call, so a turn with
// four parallel `read` calls reached the client as a single unparseable arguments blob and every
// read failed. Indexes must therefore be unique.
// @preconditions None.
// @invariants Sequential calls return 0,1,2,…; concurrent calls never repeat an index.
func TestToolCallIndexerUniquePerTurn(t *testing.T) {
	var idx ToolCallIndexer
	for want := 0; want < 5; want++ {
		if got := idx.Next(); got != want {
			t.Fatalf("Next() = %d, want %d", got, want)
		}
	}

	var concurrent ToolCallIndexer
	const n = 64
	seen := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			seen[slot] = concurrent.Next()
		}(i)
	}
	wg.Wait()

	uniq := make(map[int]struct{}, n)
	for _, v := range seen {
		if _, dup := uniq[v]; dup {
			t.Fatalf("duplicate tool call index %d across concurrent calls", v)
		}
		uniq[v] = struct{}{}
	}
	if len(uniq) != n {
		t.Fatalf("got %d distinct indexes, want %d", len(uniq), n)
	}
}
