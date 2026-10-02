package v1

import (
	"sort"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// groupChangesByTitle buckets change entries by their stable title. Entries
// inside one bucket are ordered by sequence ascending, with category and
// description breaking exact-sequence ties so the pairing below is
// deterministic. Historical records may carry several entries sharing a
// title, so every entry is retained instead of collapsing to the last one.
func groupChangesByTitle(entries []store.ChangeEntry) map[string][]store.ChangeEntry {
	grouped := make(map[string][]store.ChangeEntry, len(entries))
	for _, entry := range entries {
		grouped[entry.Title] = append(grouped[entry.Title], entry)
	}
	for title := range grouped {
		group := grouped[title]
		sortChangeGroup(group)
		grouped[title] = group
	}
	return grouped
}

// sortedChangeTitles returns the title keys of a grouping in ascending order.
func sortedChangeTitles(grouped map[string][]store.ChangeEntry) []string {
	titles := make([]string, 0, len(grouped))
	for title := range grouped {
		titles = append(titles, title)
	}
	sort.Strings(titles)
	return titles
}

// lessChangeEntry orders entries within one title group: sequence ascending,
// then category and description as deterministic tie-breakers.
func lessChangeEntry(a, b store.ChangeEntry) bool {
	if a.Sequence != b.Sequence {
		return a.Sequence < b.Sequence
	}
	if a.Category != b.Category {
		return a.Category < b.Category
	}
	return a.Description < b.Description
}

// sortChangeGroup orders one title group by lessChangeEntry.
func sortChangeGroup(group []store.ChangeEntry) {
	sort.SliceStable(group, func(i, j int) bool {
		return lessChangeEntry(group[i], group[j])
	})
}

// titlePair is one positional match of same-titled change entries across two
// sides. Within a title group the entries are paired by sequence order; extra
// entries on either side leave the missing pointer nil.
type titlePair struct {
	Title string
	Left  *store.ChangeEntry
	Right *store.ChangeEntry
}

// pairChangesByTitle groups both sides by title and pairs the entries of each
// title group position by position, in ascending sequence order. Results are
// emitted title by title in ascending order, so repeated queries over the
// same data are stable.
func pairChangesByTitle(leftEntries, rightEntries []store.ChangeEntry) []titlePair {
	leftGrouped := groupChangesByTitle(leftEntries)
	rightGrouped := groupChangesByTitle(rightEntries)
	titles := sortedChangeTitles(leftGrouped)
	for _, title := range sortedChangeTitles(rightGrouped) {
		if _, present := leftGrouped[title]; !present {
			titles = append(titles, title)
		}
	}
	pairs := make([]titlePair, 0)
	for _, title := range titles {
		leftGroup := leftGrouped[title]
		rightGroup := rightGrouped[title]
		count := len(leftGroup)
		if len(rightGroup) > count {
			count = len(rightGroup)
		}
		for i := 0; i < count; i++ {
			pair := titlePair{Title: title}
			if i < len(leftGroup) {
				entry := leftGroup[i]
				pair.Left = &entry
			}
			if i < len(rightGroup) {
				entry := rightGroup[i]
				pair.Right = &entry
			}
			pairs = append(pairs, pair)
		}
	}
	return pairs
}

// containsChangeTitle reports whether any change entry carries the title.
func containsChangeTitle(entries []store.ChangeEntry, title string) bool {
	for i := range entries {
		if entries[i].Title == title {
			return true
		}
	}
	return false
}

// firstChangeByTitle returns the lowest-sequence entry carrying the title.
// The sequence-ordered grouping keeps the choice deterministic when a
// historical record repeats a title.
func firstChangeByTitle(entries []store.ChangeEntry, title string) (store.ChangeEntry, bool) {
	group, present := groupChangesByTitle(entries)[title]
	if !present {
		return store.ChangeEntry{}, false
	}
	return group[0], true
}

// changeViewOrderKey is the stable title/sequence/category/description key
// used to order change views. Title and sequence are the documented keys;
// category and description only break ties between historical entries that
// repeat both.
func changeViewOrderKey(entry changeEntryView) string {
	return entry.Title + "\x00" + padSequence(entry.Sequence) +
		"\x00" + entry.Category + "\x00" + entry.Description
}
