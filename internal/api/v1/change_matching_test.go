package v1

import (
	"testing"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

func TestPairChangesByTitleGroupsAndPairsBySequence(t *testing.T) {
	left := []store.ChangeEntry{
		{Sequence: 2, Category: "feature", Title: "b", Description: "b2"},
		{Sequence: 1, Category: "feature", Title: "a", Description: "a1"},
		{Sequence: 2, Category: "fix", Title: "a", Description: "a2"},
	}
	right := []store.ChangeEntry{
		{Sequence: 3, Category: "feature", Title: "a", Description: "a3"},
		{Sequence: 1, Category: "feature", Title: "a", Description: "a1"},
		{Sequence: 1, Category: "feature", Title: "c", Description: "c1"},
	}
	pairs := pairChangesByTitle(left, right)
	got := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		marker := pair.Title + ":"
		if pair.Left != nil {
			marker += "L"
		}
		if pair.Right != nil {
			marker += "R"
		}
		got = append(got, marker)
	}
	want := []string{"a:LR", "a:LR", "b:L", "c:R"}
	if len(got) != len(want) {
		t.Fatalf("pairs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pairs = %v, want %v", got, want)
		}
	}
}

func TestPairChangesByTitleIsStableAcrossRepeatedCalls(t *testing.T) {
	left := []store.ChangeEntry{
		{Sequence: 1, Category: "x", Title: "z", Description: "1"},
		{Sequence: 1, Category: "x", Title: "z", Description: "2"},
	}
	first := pairChangesByTitle(left, nil)
	second := pairChangesByTitle(left, nil)
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("pairs = %v / %v", first, second)
	}
	for i := range first {
		if first[i].Left.Description != second[i].Left.Description {
			t.Fatalf("repeated pairing reordered entries: %v vs %v", first, second)
		}
	}
}
