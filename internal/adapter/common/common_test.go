package common

import "testing"

func TestSplitModel(t *testing.T) {
	for _, c := range []struct {
		in, m, e string
		bad      bool
	}{
		{"claude-sonnet-5-5@medium", "claude-sonnet-5-5", "medium", false},
		{"claude-haiku-5-5", "claude-haiku-5-5", "", false},
		{"x@ultra", "", "", true},
		{"@low", "", "", true},
	} {
		m, e, err := SplitModel(c.in)
		if (err != nil) != c.bad || m != c.m || e != c.e {
			t.Errorf("%q: got %q %q %v", c.in, m, e, err)
		}
	}
}
