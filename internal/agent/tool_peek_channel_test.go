package agent

import (
	"reflect"
	"testing"
)

// TestSampleIndices covers 缺点八 peek head/middle/tail sampling.
func TestSampleIndices(t *testing.T) {
	cases := []struct {
		name string
		n, k int
		want []int
	}{
		{"empty", 0, 5, nil},
		{"k<=0", 3, 0, nil},
		{"n<k returns all", 3, 5, []int{0, 1, 2}},
		{"n==k returns all", 5, 5, []int{0, 1, 2, 3, 4}},
		{"n>k head+tail included, evenly spaced", 9, 5, []int{0, 2, 4, 6, 8}},
		{"n=10 k=5", 10, 5, []int{0, 2, 4, 6, 9}},
		{"n=100 k=5 spans full range", 100, 5, []int{0, 24, 49, 74, 99}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sampleIndices(tc.n, tc.k)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("sampleIndices(%d,%d) = %v, want %v", tc.n, tc.k, got, tc.want)
			}
			// Invariants for the n>k case: first and last always present, distinct, in range.
			if tc.n > tc.k && len(got) > 0 {
				if got[0] != 0 {
					t.Errorf("head not included: %v", got)
				}
				if got[len(got)-1] != tc.n-1 {
					t.Errorf("tail not included: %v", got)
				}
				seen := map[int]bool{}
				for _, i := range got {
					if i < 0 || i >= tc.n {
						t.Errorf("index %d out of range [0,%d)", i, tc.n)
					}
					if seen[i] {
						t.Errorf("duplicate index %d in %v", i, got)
					}
					seen[i] = true
				}
			}
		})
	}
}
