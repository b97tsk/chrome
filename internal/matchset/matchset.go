package matchset

type atom [8]uint32

func (t *atom) Add(by byte) {
	idx := by / 32
	bit := uint32(1) << (by - idx*32)
	t[idx] |= bit
}

func (t *atom) Inverse() {
	for i, u32 := range t {
		t[i] = ^u32
	}
}

func (t atom) Contains(by byte) bool {
	idx := by / 32
	bit := uint32(1) << (by - idx*32)
	return t[idx]&bit != 0
}

const specialDot = 256

type MatchSet struct {
	patterns   [][]uint16
	knownAtoms map[atom]uint16
	atomFromID map[uint16]atom
	dataFromID map[uint32]interface{}
	nextAtomID uint16
	nextDataID uint32
}

func (set *MatchSet) lazyInit() {
	if set.knownAtoms != nil {
		return
	}
	set.knownAtoms = make(map[atom]uint16)
	set.atomFromID = make(map[uint16]atom)
	set.dataFromID = make(map[uint32]interface{})
	set.nextAtomID = specialDot + 1
	set.nextDataID = 1
}

func (set *MatchSet) getNextAtomID() uint16 {
	id := set.nextAtomID
	if id == 0 {
		panic("too many atoms")
	}
	set.nextAtomID++
	return id
}

func (set *MatchSet) getNextDataID() uint32 {
	id := set.nextDataID
	if id == 0 {
		panic("too many rules")
	}
	set.nextDataID++
	return id
}

func atomFromGroup(bytes []byte) (atom, []byte) {
	var (
		char                 byte // Last character read.
		charRead             bool // A character is read.
		charRange            bool // A character range expected.
		charSet              atom // Set of characters read.
		charSetInversed      bool
		charSetInversedMaybe bool
	)
	for i, by := range bytes {
		switch by {
		case ']':
			if !charRange && i > 0 {
				if charRead {
					charSet.Add(char)
				}
				if charSetInversed {
					charSet.Inverse()
				}
				return charSet, bytes[i+1:]
			}
		case '-':
			if charRead && !charRange {
				charRange = true
				continue
			}
		}
		if charSetInversedMaybe {
			// Skip first '^' character.
			charRead = false
			charSetInversed = true
			charSetInversedMaybe = false
		}
		if i == 0 && by == '^' {
			charSetInversedMaybe = true
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
	if charSetInversed {
		charSet.Inverse()
	}
	return charSet, nil
}

func (set *MatchSet) readAtom(bytes []byte) (uint16, []byte) {
	if bytes[0] == '[' {
		t, bytesLeft := atomFromGroup(bytes[1:])
		if known, yes := set.knownAtoms[t]; yes {
			return known, bytesLeft
		}
		atomID := set.getNextAtomID()
		set.knownAtoms[t] = atomID
		set.atomFromID[atomID] = t
		return atomID, bytesLeft
	}
	return uint16(bytes[0]), bytes[1:]
}

func (set *MatchSet) parse(bytes []byte) []uint16 {
	var (
		atoms        []uint16
		currentAtom  uint16
		lastReadAtom uint16
	)
	for len(bytes) > 0 {
		currentAtom, bytes = set.readAtom(bytes)
		switch currentAtom {
		case '*':
			if lastReadAtom != '*' {
				lastReadAtom = '*'
				atoms = append(atoms, lastReadAtom)
			}
		case '?':
			if lastReadAtom == '*' {
				atoms[len(atoms)-1] = '?'
				atoms = append(atoms, '*')
			} else {
				lastReadAtom = '?'
				atoms = append(atoms, lastReadAtom)
			}
		default:
			lastReadAtom = currentAtom
			atoms = append(atoms, lastReadAtom)
		}
	}
	if len(atoms) == 0 {
		// Empty pattern matches any characters.
		return []uint16{specialDot}
	}
	if atoms[0] == '.' {
		// Starting with '.' means it could match any characters at the beginning.
		atoms[0] = specialDot
	}
	if atoms[len(atoms)-1] == '.' {
		// Ending with '.' means it could match any characters at the end.
		atoms[len(atoms)-1] = specialDot
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
		if atoms[i] == '*' && atoms[i+1] == '?' {
			atoms[i], atoms[i+1] = atoms[i+1], atoms[i]
		}
	}
	dataID := set.getNextDataID()
	set.dataFromID[dataID] = data
	atoms = append(atoms, uint16(dataID), uint16(dataID>>16))
	if len(atoms) < cap(atoms) {
		// Reduce memory use.
		new := make([]uint16, len(atoms))
		copy(new, atoms)
		atoms = new
	}
	set.patterns = append(set.patterns, atoms[:len(atoms)-2])
}

func (set *MatchSet) Empty() bool {
	return len(set.patterns) == 0
}

func (set *MatchSet) Match(source string) (matches []interface{}) {
	if set.Empty() {
		return
	}
	set.MatchFunc(source, func(data interface{}) {
		matches = append(matches, data)
	})
	return
}

func (set *MatchSet) MatchCount(source string) (count int) {
	if set.Empty() {
		return
	}
	set.MatchFunc(source, func(data interface{}) { count++ })
	return
}

func (set *MatchSet) MatchFunc(source string, accumulate func(interface{})) {
	bytes := []byte(source)
	// Also reverse source bytes, since all patterns are reversed.
	for i, j := 0, len(bytes)-1; i < j; i, j = i+1, j-1 {
		bytes[i], bytes[j] = bytes[j], bytes[i]
	}
	var patterns, matches [][]uint16
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
	atoms []uint16,
	bytes []byte, i int,
	matches [][]uint16,
	accumulate func(interface{}),
) [][]uint16 {
	switch {
	case atoms[0] == specialDot:
		if len(atoms) == 1 {
			if i == 0 || bytes[i] == '.' {
				accumulate(set.extractData(atoms))
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
	case atoms[0] == '*':
		if len(atoms) > 1 {
			matches = set.checkPattern(atoms[1:], bytes[i:], 0, matches, accumulate)
		}
		switch bytes[i] {
		case '.', ':':
			return matches // '*' does not match '.' or ':'.
		}
		if i+1 == len(bytes) {
			if len(atoms) == 1 {
				accumulate(set.extractData(atoms))
			}
			return matches
		}
		matches = append(matches, atoms)
	case atoms[0] == '?':
		switch bytes[i] {
		case '.', ':':
			return matches // '?' does not match '.' or ':'.
		}
		fallthrough
	case atoms[0] == uint16(bytes[i]) || set.atomFromID[atoms[0]].Contains(bytes[i]):
		if i+1 == len(bytes) {
			switch len(atoms) {
			case 1:
				accumulate(set.extractData(atoms))
			case 2:
				switch atoms[1] {
				case specialDot, '*':
					accumulate(set.extractData(atoms))
				}
			case 3:
				if atoms[1] == '*' && atoms[2] == specialDot {
					accumulate(set.extractData(atoms))
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

func (set *MatchSet) extractData(atoms []uint16) interface{} {
	n := len(atoms)
	atoms = atoms[:n+2]
	lo, hi := atoms[n], atoms[n+1]
	dataID := uint32(lo) + uint32(hi)<<16
	return set.dataFromID[dataID]
}
