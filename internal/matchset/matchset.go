package matchset

type atom [8]uint32

func (t *atom) Add(by byte) {
	idx := by / 32
	bit := uint32(1) << (by - idx*32)
	t[idx] |= bit
}

func (t *atom) Contains(by byte) bool {
	idx := by / 32
	bit := uint32(1) << (by - idx*32)
	return t[idx]&bit != 0
}

func (t *atom) Reverse() {
	for i, u32 := range t {
		t[i] = ^u32
	}
}

var (
	basicAtoms     [256]atom
	atomSpecialDot atom
)

func init() {
	for i := range basicAtoms {
		basicAtoms[i].Add(byte(i))
	}
}

type MatchSet struct {
	patterns   [][]*atom
	knownAtoms map[atom]*atom
	dataset    map[*atom]interface{}
}

func (set *MatchSet) lazyInit() {
	if set.knownAtoms != nil {
		return
	}
	set.knownAtoms = make(map[atom]*atom)
	for i, t := range basicAtoms {
		set.knownAtoms[t] = &basicAtoms[i]
	}
	set.dataset = make(map[*atom]interface{})
}

func atomFromGroup(bytes []byte) (atom, []byte) {
	var (
		char                 byte // Last character read.
		charRead             bool // A character is read.
		charRange            bool // A character range expected.
		charSet              atom // Set of characters read.
		charSetReversed      bool
		charSetReversedMaybe bool
	)
	for i, by := range bytes {
		switch by {
		case ']':
			if !charRange && i > 0 {
				if charRead {
					charSet.Add(char)
				}
				if charSetReversed {
					charSet.Reverse()
				}
				return charSet, bytes[i+1:]
			}
		case '-':
			if charRead && !charRange {
				charRange = true
				continue
			}
		}
		if charSetReversedMaybe {
			// Skip first '^' character.
			charRead = false
			charSetReversed = true
			charSetReversedMaybe = false
		}
		if i == 0 && by == '^' {
			charSetReversedMaybe = true
		}
		if charRange {
			for ; char <= by; char++ {
				charSet.Add(char)
			}
			charRead = false
			charRange = false
		} else {
			if charRead {
				charSet.Add(char)
			}
			char = by
			charRead = true
		}
	}
	if charRead {
		charSet.Add(char)
	}
	if charSetReversed {
		charSet.Reverse()
	}
	return charSet, nil
}

func (set *MatchSet) readAtom(bytes []byte) (*atom, []byte) {
	if bytes[0] == '[' {
		t, bytesLeft := atomFromGroup(bytes[1:])
		if known, yes := set.knownAtoms[t]; yes {
			return known, bytesLeft
		}
		set.knownAtoms[t] = &t
		return &t, bytesLeft
	}
	var t atom
	t.Add(bytes[0])
	return set.knownAtoms[t], bytes[1:]
}

func (set *MatchSet) parse(bytes []byte) []*atom {
	var (
		atoms        []*atom
		currentAtom  *atom
		lastReadAtom *atom
	)
	for len(bytes) > 0 {
		currentAtom, bytes = set.readAtom(bytes)
		switch currentAtom {
		case &basicAtoms['*']:
			if lastReadAtom != &basicAtoms['*'] {
				lastReadAtom = &basicAtoms['*']
				atoms = append(atoms, lastReadAtom)
			}
		case &basicAtoms['?']:
			if lastReadAtom == &basicAtoms['*'] {
				atoms[len(atoms)-1] = &basicAtoms['?']
				atoms = append(atoms, &basicAtoms['*'])
			} else {
				lastReadAtom = &basicAtoms['?']
				atoms = append(atoms, lastReadAtom)
			}
		default:
			lastReadAtom = currentAtom
			atoms = append(atoms, lastReadAtom)
		}
	}
	if len(atoms) == 0 {
		// Empty pattern matches any characters.
		return []*atom{&atomSpecialDot}
	}
	if atoms[0] == &basicAtoms['.'] {
		// Starting with '.' means it could match any characters at the beginning.
		atoms[0] = &atomSpecialDot
	}
	if atoms[len(atoms)-1] == &basicAtoms['.'] {
		// Ending with '.' means it could match any characters at the end.
		atoms[len(atoms)-1] = &atomSpecialDot
	}
	return atoms
}

func (set *MatchSet) Add(pattern string, data interface{}) {
	set.lazyInit()
	atoms := set.parse([]byte(pattern))
	// Reverse atoms for better performance, because in practice,
	// patterns like ".abc" are much more frequently used than
	// patterns like "abc.".
	for i, j := 0, len(atoms)-1; i < j; i, j = i+1, j-1 {
		atoms[i], atoms[j] = atoms[j], atoms[i]
	}
	for i := 0; i < len(atoms)-1; i++ {
		if atoms[i] == &basicAtoms['*'] && atoms[i+1] == &basicAtoms['?'] {
			atoms[i], atoms[i+1] = atoms[i+1], atoms[i]
		}
	}
	var dataAtom atom
	set.dataset[&dataAtom] = data
	atoms = append(atoms, &dataAtom)
	set.patterns = append(set.patterns, atoms[:len(atoms)-1])
}

func (set *MatchSet) Match(source string) (matches []interface{}) {
	set.MatchFunc(source, func(data interface{}) { matches = append(matches, data) })
	return
}

func (set *MatchSet) MatchCount(source string) (count int) {
	set.MatchFunc(source, func(data interface{}) { count++ })
	return
}

func (set *MatchSet) MatchFunc(source string, accumulate func(interface{})) {
	bytes := []byte(source)
	// Also reverse source bytes, since all patterns are reversed.
	for i, j := 0, len(bytes)-1; i < j; i, j = i+1, j-1 {
		bytes[i], bytes[j] = bytes[j], bytes[i]
	}
	var patterns, matches [][]*atom
	for _, atoms := range set.patterns {
		patterns = patterns[:0]
		patterns = append(patterns, atoms)
		for i := range bytes {
			matches = matches[:0]
			for _, atoms := range patterns {
				matches = set.checkPattern(atoms, bytes, i, matches, accumulate)
			}
			patterns, matches = matches, patterns
			if len(patterns) == 0 {
				break
			}
		}
	}
}

func (set *MatchSet) checkPattern(
	atoms []*atom,
	bytes []byte, i int,
	matches [][]*atom,
	accumulate func(interface{}),
) [][]*atom {
	switch {
	case atoms[0] == &atomSpecialDot:
		if len(atoms) == 1 {
			if i == 0 || bytes[i] == '.' {
				accumulate(set.dataset[atoms[:2][1]])
			}
			return matches
		}
		if i == 0 {
			matches = set.checkPattern(atoms[1:], bytes[i:], 0, matches, accumulate)
		}
		if bytes[i] == '.' {
			matches = append(matches, atoms[1:])
		}
		matches = append(matches, atoms)
	case atoms[0] == &basicAtoms['*']:
		if len(atoms) > 1 {
			matches = set.checkPattern(atoms[1:], bytes[i:], 0, matches, accumulate)
		}
		switch bytes[i] {
		case '.', ':':
			return matches // '*' does not match '.' or ':'.
		}
		if i+1 == len(bytes) {
			if len(atoms) == 1 {
				accumulate(set.dataset[atoms[:2][1]])
			}
			return matches
		}
		matches = append(matches, atoms)
	case atoms[0] == &basicAtoms['?']:
		switch bytes[i] {
		case '.', ':':
			return matches // '?' does not match '.' or ':'.
		}
		fallthrough
	case atoms[0].Contains(bytes[i]):
		if i+1 == len(bytes) {
			switch len(atoms) {
			case 1:
				accumulate(set.dataset[atoms[:2][1]])
			case 2:
				switch atoms[1] {
				case &atomSpecialDot, &basicAtoms['*']:
					accumulate(set.dataset[atoms[:3][2]])
				}
			case 3:
				if atoms[1] == &basicAtoms['*'] && atoms[2] == &atomSpecialDot {
					accumulate(set.dataset[atoms[:4][3]])
				}
			}
			return matches
		}
		if len(atoms) == 1 {
			return matches
		}
		matches = append(matches, atoms[1:])
	}
	return matches
}
