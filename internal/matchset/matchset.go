package matchset

import (
	"unicode/utf8"
)

type charset [8]uint32

func (set *charset) Add(by byte) {
	idx := by / 32
	bit := uint32(1) << (by - idx*32)
	set[idx] |= bit
}

func (set *charset) Invert() {
	for i, u32 := range set {
		set[i] = ^u32
	}
}

func (set charset) Contains(by byte) bool {
	idx := by / 32
	bit := uint32(1) << (by - idx*32)

	return set[idx]&bit != 0
}

type atom = rune

type pattern struct {
	atoms []byte
	data  interface{}
}

const magicDot = 0

type MatchSet struct {
	patterns      []pattern
	nextAtom      atom
	charSetToAtom map[charset]atom
	atomToCharSet map[atom]charset
	atomsBuffer   []atom
}

func charSetFromGroup(bytes []byte) (charset, []byte) {
	var (
		char                 byte    // Last character read.
		charRead             bool    // A character is read.
		charRange            bool    // A character range expected.
		charSet              charset // Set of characters read.
		charSetInverted      bool
		charSetMayBeInverted bool
	)

	for i, by := range bytes {
		switch by {
		case ']':
			if !charRange && i > 0 {
				if charRead {
					charSet.Add(char)
				}

				if charSetInverted {
					charSet.Invert()
				}

				return charSet, bytes[i+1:]
			}
		case '-':
			if charRead && !charRange {
				charRange = true
				continue
			}
		}

		if charSetMayBeInverted {
			// Skip first '^' character.
			charRead = false
			charSetInverted = true
			charSetMayBeInverted = false
		}

		if i == 0 && by == '^' {
			charSetMayBeInverted = true
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

	if charSetInverted {
		charSet.Invert()
	}

	return charSet, nil
}

func (set *MatchSet) readAtom(bytes []byte) (atom, []byte) {
	if bytes[0] == '[' {
		charSet, bytesLeft := charSetFromGroup(bytes[1:])
		if atom, ok := set.charSetToAtom[charSet]; ok {
			return atom, bytesLeft
		}

		if set.nextAtom == 0 {
			set.nextAtom = 256
			set.charSetToAtom = make(map[charset]atom)
			set.atomToCharSet = make(map[atom]charset)
		}

		atom := set.nextAtom
		set.nextAtom++

		set.charSetToAtom[charSet] = atom
		set.atomToCharSet[atom] = charSet

		return atom, bytesLeft
	}

	return atom(bytes[0]), bytes[1:]
}

func (set *MatchSet) parse(bytes []byte) []atom {
	var (
		currentAtom  atom
		lastReadAtom atom
	)

	atoms := set.atomsBuffer[:0]

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

	set.atomsBuffer = atoms

	if len(atoms) == 0 {
		// Empty pattern matches any characters.
		return []atom{magicDot}
	}

	if atoms[0] == '.' {
		// Starting with '.' means it could match any characters at the beginning.
		atoms[0] = magicDot
	}

	if atoms[len(atoms)-1] == '.' {
		// Ending with '.' means it could match any characters at the end.
		atoms[len(atoms)-1] = magicDot
	}

	return atoms
}

func (set *MatchSet) Add(patt string, data interface{}) {
	atoms := set.parse([]byte(patt))

	// Reverse atoms for better performance, because in practice,
	// patterns like ".abc" are much more frequently used than
	// patterns like "abc.".
	{
		for i, j := 0, len(atoms)-1; i < j; i, j = i+1, j-1 {
			atoms[i], atoms[j] = atoms[j], atoms[i]
		}

		for i := 0; i < len(atoms)-1; i++ {
			if atoms[i] == '*' && atoms[i+1] == '?' {
				atoms[i], atoms[i+1] = atoms[i+1], atoms[i]
			}
		}
	}

	bytes := []byte(string(atoms)) // For reducing memory usage purpose.
	set.patterns = append(set.patterns, pattern{bytes, data})
}

func (set *MatchSet) Empty() bool {
	return len(set.patterns) == 0
}

func (set *MatchSet) Match(source string, accumulate func(interface{}) bool) {
	if set.Empty() {
		return
	}

	bytes := []byte(source)

	// Also reverse source bytes, since all patterns are reversed.
	for i, j := 0, len(bytes)-1; i < j; i, j = i+1, j-1 {
		bytes[i], bytes[j] = bytes[j], bytes[i]
	}

	var ok bool

	var patterns, matches []pattern

	for _, patt := range set.patterns {
		patterns = patterns[:0]
		patterns = append(patterns, patt)

		for i := range bytes {
			matches = matches[:0]

			for _, patt := range patterns {
				if matches, ok = set.checkPattern(patt, bytes, i, matches, accumulate); !ok {
					return
				}
			}

			if len(matches) == 0 {
				break
			}

			patterns, matches = matches, patterns
		}
	}
}

func (set *MatchSet) checkPattern(
	patt pattern,
	bytes []byte, i int,
	matches []pattern,
	accumulate func(interface{}) bool,
) (_ []pattern, ok bool) {
	atom, size := utf8.DecodeRune(patt.atoms)
	nextpatt := pattern{patt.atoms[size:], patt.data}

	switch {
	case atom == magicDot:
		if len(nextpatt.atoms) == 0 {
			if i == 0 || bytes[i] == '.' {
				if !accumulate(patt.data) {
					return nil, false
				}
			}

			return matches, true
		}

		if i == 0 {
			if matches, ok = set.checkPattern(nextpatt, bytes[i:], 0, matches, accumulate); !ok {
				return nil, false
			}
		}

		if bytes[i] == '.' {
			matches = append(matches, nextpatt)
		}

		matches = append(matches, patt)
	case atom == '*':
		if len(nextpatt.atoms) > 0 {
			if matches, ok = set.checkPattern(nextpatt, bytes[i:], 0, matches, accumulate); !ok {
				return nil, false
			}
		}

		switch bytes[i] {
		case '.', ':':
			return matches, true // '*' does not match '.' or ':'.
		}

		if i+1 == len(bytes) {
			if len(nextpatt.atoms) == 0 {
				if !accumulate(patt.data) {
					return nil, false
				}
			}

			return matches, true
		}

		matches = append(matches, patt)
	case atom == '?':
		switch bytes[i] {
		case '.', ':':
			return matches, true // '?' does not match '.' or ':'.
		}

		fallthrough
	case atom == rune(bytes[i]) || set.atomToCharSet[atom].Contains(bytes[i]):
		if i+1 == len(bytes) {
			switch len(nextpatt.atoms) {
			case 0:
				if !accumulate(patt.data) {
					return nil, false
				}
			case 1:
				switch nextpatt.atoms[0] {
				case magicDot, '*':
					if !accumulate(patt.data) {
						return nil, false
					}
				}
			case 2:
				if nextpatt.atoms[0] == '*' && nextpatt.atoms[1] == magicDot {
					if !accumulate(patt.data) {
						return nil, false
					}
				}
			}

			return matches, true
		}

		if len(nextpatt.atoms) == 0 {
			return matches, true
		}

		matches = append(matches, nextpatt)
	}

	return matches, true
}

func (set *MatchSet) MatchAll(source string) (matches []interface{}) {
	set.Match(source, func(data interface{}) bool {
		matches = append(matches, data)
		return true
	})

	return
}

func (set *MatchSet) Test(source string) (ok bool) {
	set.Match(source, func(data interface{}) bool {
		ok = true
		return false
	})

	return
}
