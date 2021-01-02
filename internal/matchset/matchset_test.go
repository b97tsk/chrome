package matchset_test

import (
	"testing"

	"github.com/b97tsk/chrome/internal/matchset"
)

func TestMatchSet(t *testing.T) {
	tests := []struct {
		source  string
		pattern string
		matched bool
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

	for _, tt := range tests {
		var set matchset.MatchSet

		set.Add(tt.pattern, struct{}{})

		if tt.matched != set.Test(tt.source) {
			if tt.matched {
				t.Error(tt.source, "SHOULD match", tt.pattern)
			} else {
				t.Error(tt.source, "SHOULD NOT match", tt.pattern)
			}
		}
	}
}
