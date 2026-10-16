package config

import "testing"

func TestSameAccountSet(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"reordered", []string{"a", "b"}, []string{"b", "a"}, true},
		{"duplicate", []string{"a"}, []string{"a", "a"}, true},
		{"added", []string{"a"}, []string{"a", "b"}, false},
		{"removed", []string{"a", "b"}, []string{"a"}, false},
		{"replaced", []string{"a"}, []string{"b"}, false},
		{"both empty", nil, []string{}, true},
		{"mixed case", []string{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}, []string{"AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"}, true},
		{"mixed case duplicate", []string{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}, []string{"AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}, true},
	}
	for _, tc := range cases {
		if got := sameAccountSet(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
