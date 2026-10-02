package v1

import (
	"sort"
	"strconv"
	"strings"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// matchedChangePair aligns the change entries carrying one stable title
// across two compared releases. Each side may contribute several entries
// sharing that title (legacy records created before the write-time
// uniqueness rule); entries pair positionally after ordering by sequence.
// Left is nil when the title exists only on the right side (added) and vice
// versa (missing).
type matchedChangePair struct {
	Title string
	Left  *store.ChangeEntry
	Right *store.ChangeEntry
}

// groupEntriesByTitle indexes every change entry under its stable title.
// Records with repeated titles keep all of their entries instead of letting
// a later entry overwrite an earlier one.
func groupEntriesByTitle(entries []store.ChangeEntry) map[string][]store.ChangeEntry {
	grouped := make(map[string][]store.ChangeEntry, len(entries))
	for i := range entries {
		entry := entries[i]
		grouped[entry.Title] = append(grouped[entry.Title], entry)
	}
	return grouped
}

// matchChangeEntries groups both sides by title and pairs entries of the
// same title positionally in ascending sequence order. Surplus entries on
// either side are returned as one-sided pairs, so repeated titles never
// collapse into a single comparison.
func matchChangeEntries(leftEntries, rightEntries []store.ChangeEntry) []matchedChangePair {
	leftGroups := groupEntriesByTitle(leftEntries)
	rightGroups := groupEntriesByTitle(rightEntries)
	titles := make(map[string]struct{}, len(leftGroups)+len(rightGroups))
	for title := range leftGroups {
		titles[title] = struct{}{}
	}
	for title := range rightGroups {
		titles[title] = struct{}{}
	}
	pairs := make([]matchedChangePair, 0, len(titles))
	for title := range titles {
		leftSide := sortedTitleGroup(leftGroups[title])
		rightSide := sortedTitleGroup(rightGroups[title])
		count := len(leftSide)
		if len(rightSide) > count {
			count = len(rightSide)
		}
		for i := 0; i < count; i++ {
			pair := matchedChangePair{Title: title}
			if i < len(leftSide) {
				entry := leftSide[i]
				pair.Left = &entry
			}
			if i < len(rightSide) {
				entry := rightSide[i]
				pair.Right = &entry
			}
			pairs = append(pairs, pair)
		}
	}
	return pairs
}

// sortedTitleGroup returns the entries of one title group ordered by sequence
// ascending, keeping write order for same-sequence ties.
func sortedTitleGroup(entries []store.ChangeEntry) []store.ChangeEntry {
	sorted := make([]store.ChangeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Sequence < sorted[j].Sequence
	})
	return sorted
}

// sameEntry reports whether two paired entries agree on sequence, category
// and description. Titles already match by construction.
func sameEntry(a, b store.ChangeEntry) bool {
	return a.Sequence == b.Sequence && a.Category == b.Category && a.Description == b.Description
}

// containsChangeTitle reports whether an entry carrying the title exists in
// the record. Presence is all callers need, so repeated titles must not
// overwrite each other while answering.
func containsChangeTitle(entries []store.ChangeEntry, title string) bool {
	for i := range entries {
		if entries[i].Title == title {
			return true
		}
	}
	return false
}

// sortChangeViewsByTitle orders entry views by title and then sequence,
// keeping identical repeated titles in a stable, deterministic order.
func sortChangeViewsByTitle(entries []changeEntryView) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Title != entries[j].Title {
			return entries[i].Title < entries[j].Title
		}
		return entries[i].Sequence < entries[j].Sequence
	})
}

// sortChangedEntryViews applies the title-then-sequence ordering to paired
// entry views keyed by their left-side entry.
func sortChangedEntryViews(pairs []changedEntryView) {
	sort.SliceStable(pairs, func(i, j int) bool {
		return changedEntryOrder(pairs[i].Left) < changedEntryOrder(pairs[j].Left)
	})
}

func changedEntryOrder(entry changeEntryView) string {
	return entry.Title + "\x00" + padSequence(entry.Sequence)
}

// padSequence makes sequence ordering lexical regardless of digit count.
func padSequence(sequence int) string {
	digits := strconv.Itoa(sequence)
	return strings.Repeat("0", 12-len(digits)) + digits
}
