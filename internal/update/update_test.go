package update

import (
	"reflect"
	"testing"
)

func TestCountdownMarks(t *testing.T) {
	cases := map[int][]int{15: {15, 10, 5, 2, 1}, 5: {5, 2, 1}, 1: {1}, 10: {10, 5, 2, 1}, 0: nil, 30: {30, 10, 5, 2, 1}}
	for n, want := range cases {
		if got := CountdownMarks(n); !reflect.DeepEqual(got, want) {
			t.Errorf("%d: got %v want %v", n, got, want)
		}
	}
}
