package api

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
)

// compareVersions orders version strings deterministically: dot-separated
// segments compare numerically when both parse as integers and lexically
// otherwise; a shared prefix sorts before its extension.
func compareVersions(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		if aParts[i] == bParts[i] {
			continue
		}
		aNum, aErr := strconv.Atoi(aParts[i])
		bNum, bErr := strconv.Atoi(bParts[i])
		if aErr == nil && bErr == nil && aNum != bNum {
			return cmp.Compare(aNum, bNum)
		}
		return strings.Compare(aParts[i], bParts[i])
	}
	return cmp.Compare(len(aParts), len(bParts))
}

// sortVersions returns the values sorted with compareVersions.
func sortVersions(versions []string) []string {
	slices.SortFunc(versions, compareVersions)
	return versions
}
