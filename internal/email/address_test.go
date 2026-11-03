package email

import "testing"

func TestSameAddress(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Admin@Example.com", " admin@example.COM ", true},
		{"admin@example.com", "other@example.com", false},
		{"admin@example.com", "admİn@example.com", false},
		{"admin@example.com", "admin@example.com", true},
		{"s@example.com", "\u017f@example.com", false},
		{"k@example.com", "\u212a@example.com", false},
		{"i@example.com", "\u0130@example.com", false},
		{"user@example.com", "u\u017fer@example.com", false},
		{"user@example.com", "user@example.co", false},
		{"", "", true},
	}
	for _, tc := range cases {
		if got := SameAddress(tc.a, tc.b); got != tc.want {
			t.Errorf("SameAddress(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestContainsAddress(t *testing.T) {
	list := []string{"a@example.com", " B@Example.com"}
	if !ContainsAddress(list, "b@example.com ") {
		t.Error("expected match")
	}
	if ContainsAddress(list, "admİn@example.com") || ContainsAddress(nil, "a@example.com") {
		t.Error("unexpected match")
	}
}
