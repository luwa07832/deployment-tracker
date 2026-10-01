package v1

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
)

// compareVersions mirrors the baseline ordering: dot-separated segments
// compare numerically when both parse as integers and lexically otherwise.
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

func sortVersions(versions []string) []string {
	slices.SortFunc(versions, compareVersions)
	return versions
}
