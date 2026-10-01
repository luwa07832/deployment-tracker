package api

import (
	"reflect"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.2.0", "1.10.0", -1},
		{"1.10.0", "1.2.0", 1},
		{"1.2", "1.2.0", -1},
		{"2.0.0", "1.9.9", 1},
		{"abc", "abd", -1},
		{"v2", "v10", 1}, // non-numeric segments compare lexically
		{"1.0.0", "2.0.0", -1},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSortVersions(t *testing.T) {
	got := sortVersions([]string{"1.10.0", "1.2.0", "2.0.0", "1.2", "0.9.0"})
	want := []string{"0.9.0", "1.2", "1.2.0", "1.10.0", "2.0.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sortVersions = %v, want %v", got, want)
	}
}
