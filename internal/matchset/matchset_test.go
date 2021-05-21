package matchset_test

import (
	"testing"

	"github.com/b97tsk/chrome/internal/matchset"
)

func TestMatchSet(t *testing.T) {
	testCases := []struct {
		Source  string
		Pattern string
		Matched bool
	}{
		{"co.uk", "co.uk", true},

		{"co.uk", "*", false},
		{"co.uk", "*.uk", true},
		{"co.uk", "co.*", true},
		{"co.uk", "*.*", true},

		{"co.uk", "*o.uk", true},
		{"co.uk", "c*.uk", true},
		{"co.uk", "co.*k", true},
		{"co.uk", "co.u*", true},

		{"co.uk", ".", true},
		{"co.uk", ".uk", true},
		{"co.uk", ".co.uk", true},
		{"co.uk", "co.", true},
		{"co.uk", "co.uk.", true},
		{"co.uk", ".co.", true},
		{"co.uk", ".uk.", true},
		{"co.uk", ".co.uk.", true},

		{"co.uk", "o.uk", false},
		{"co.uk", "co.u", false},
		{"co.uk", ".o.uk", false},
		{"co.uk", "co.u.", false},
		{"co.uk", "..uk", false},
		{"co.uk", "co..", false},

		{"co.uk", ".*", true},
		{"co.uk", ".*.*", true},
		{"co.uk", "*.", true},
		{"co.uk", "*.*.", true},
		{"co.uk", ".*.", true},
		{"co.uk", ".*.*.", true},

		{"co.uk", "?o.uk", true},
		{"co.uk", "c?.uk", true},
		{"co.uk", "co?uk", false},
		{"co.uk", "co.?k", true},
		{"co.uk", "co.u?", true},
		{"co.uk", "??.uk", true},
		{"co.uk", "co.??", true},
		{"co.uk", "c?.?k", true},
		{"co.uk", "?o.u?", true},
		{"co.uk", "??.??", true},
		{"co.uk", "?????", false},

		{"co.uk", ".??", true},
		{"co.uk", ".??.??", true},
		{"co.uk", "??.", true},
		{"co.uk", "??.??.", true},
		{"co.uk", ".??.", true},
		{"co.uk", ".??.??.", true},

		{"co.uk", "?.", false},
		{"co.uk", "???.", false},
		{"co.uk", ".?", false},
		{"co.uk", ".???", false},

		{"co.uk", ".?*", true},
		{"co.uk", ".?*.?*", true},
		{"co.uk", "?*.", true},
		{"co.uk", "?*.?*.", true},
		{"co.uk", ".?*.", true},
		{"co.uk", ".?*.?*.", true},

		{"co.uk", "co.?*", true},
		{"co.uk", "co.??*", true},
		{"co.uk", "co.???*", false},
		{"co.uk", "co.*???", false},
		{"co.uk", "?*.uk", true},
		{"co.uk", "??*.uk", true},
		{"co.uk", "???*.uk", false},
		{"co.uk", "*???.uk", false},

		{"co.uk", "co.[uk][uk]", true},
		{"co.uk", "co.[a-z][a-z]", true},
		{"co.uk", "co.[0-9][0-9]", false},
		{"co.uk", "co.[^0-9][^0-9]", true},
		{"co.uk", "co.[^0-9][^0-9]*", true},

		// missing final ']'
		{"co.uk", "co.[uk][uk", true},
		{"co.uk", "co.[^0-9][^0-9", true},

		{"a", "a", true},
		{"a", "b", false},
		{"a", "*", true},
		{"a", ".", true},
		{"a", "?", true},
		{"a", "*a", true},
		{"a", "a*", true},
		{"a", "*a*", true},
		{"a", ".a", true},
		{"a", "a.", true},
		{"a", ".a.", true},
		{"a", ".?", true},
		{"a", "?.", true},
		{"a", ".?.", true},
		{"a", ".*a", true},
		{"a", ".a*", true},
		{"a", "*a.", true},
		{"a", "a*.", true},
		{"a", "[a]", true},
		{"a", "[^a]", false},
		{"a", "", true},
		{"", "a", false},
	}

	for _, tc := range testCases {
		var set matchset.MatchSet

		set.Add(tc.Pattern, struct{}{})

		matched := set.Test(tc.Source)
		if matched != (set.MatchAll(tc.Source) != nil) {
			t.Fatal("inconsistent")
		}

		if matched != tc.Matched {
			if tc.Matched {
				t.Error(tc.Source, "SHOULD match", tc.Pattern)
			} else {
				t.Error(tc.Source, "SHOULD NOT match", tc.Pattern)
			}
		}
	}
}
