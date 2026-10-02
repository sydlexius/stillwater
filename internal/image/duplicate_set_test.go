package image

import (
	"reflect"
	"testing"
)

// pairsOf builds a symmetric pairing predicate from an explicit edge list.
func pairsOf(edges ...[2]int) func(a, b int) bool {
	set := make(map[[2]int]bool, 2*len(edges))
	for _, e := range edges {
		set[e] = true
		set[[2]int{e[1], e[0]}] = true
	}
	return func(a, b int) bool { return set[[2]int{a, b}] }
}

func TestRepresentativeDeletionSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		order []int
		edges [][2]int
		want  map[int]int
	}{
		{
			// The non-transitive chain: 0~1, 1~2, 0!~2. Naive grouping deletes
			// both 1 and 2; slot 2 is distinct from the survivor and must stay.
			name:  "chain keeps the distinct far end",
			order: []int{0, 1, 2},
			edges: [][2]int{{0, 1}, {1, 2}},
			want:  map[int]int{1: 0},
		},
		{
			// Same chain walked from the MIDDLE: 1 is preferred, so it absorbs
			// both ends, each of which it pairs with directly.
			name:  "order is the survivor policy",
			order: []int{1, 0, 2},
			edges: [][2]int{{0, 1}, {1, 2}},
			want:  map[int]int{0: 1, 2: 1},
		},
		{
			name:  "a clique collapses onto the first member",
			order: []int{2, 0, 1},
			edges: [][2]int{{0, 1}, {0, 2}, {1, 2}},
			want:  map[int]int{0: 2, 1: 2},
		},
		{
			name:  "no pairs deletes nothing",
			order: []int{0, 1, 2},
			want:  map[int]int{},
		},
		{
			name:  "empty order",
			order: nil,
			want:  map[int]int{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RepresentativeDeletionSet(tc.order, pairsOf(tc.edges...))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("RepresentativeDeletionSet(%v) = %v, want %v", tc.order, got, tc.want)
			}
		})
	}
}

// The predicate must only ever be asked about a representative that comes
// EARLIER in the walk than the member; a caller's ordering-dependent predicate
// (the platform prune's local-twin guard) relies on that.
func TestRepresentativeDeletionSet_PredicateSeesRepBeforeMember(t *testing.T) {
	t.Parallel()
	order := []int{5, 3, 9}
	pos := map[int]int{5: 0, 3: 1, 9: 2}
	RepresentativeDeletionSet(order, func(rep, member int) bool {
		if pos[rep] >= pos[member] {
			t.Errorf("paired(%d, %d) called with rep not before member", rep, member)
		}
		return false
	})
}
