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

const (
	magicDot        = iota
	magicAsterisk   // matches zero or more characters, any characters except '.' and ':'
	magicAsterisk2  // matches zero or more characters, any characters
	magicQuestion   // matches exactly one character, any character except '.' and ':'
	magicUnderscore // matches exactly one character, any character
)

type MatchSet struct {
	patterns      []pattern
	nextAtom      rune
	charSetToAtom map[charset]rune
	atomToCharSet map[rune]charset
	atomsBuffer   []rune
}

type pattern struct {
	atoms []byte
	data  any
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

	var bytesLeft []byte

	for i, by := range bytes {
		if by == ']' && i > 0 {
			bytesLeft = bytes[i+1:]
			break
		}

		if by == '-' && charRead && !charRange {
			charRange = true
			continue
		}

		if charSetMayBeInverted {
			if !charRange {
				// Skip first '^' character.
				charRead = false
				charSetInverted = true
			}

			charSetMayBeInverted = false
		}

		if charRange {
			for ; char <= by; char++ {
				charSet.Add(char)
			}

			charRead = false
			charRange = false

			continue
		}

		if charRead {
			charSet.Add(char)
		}

		char = by
		charRead = true

		if i == 0 && by == '^' {
			charSetMayBeInverted = true
		}
	}

	if charRead && !charSetMayBeInverted {
		charSet.Add(char)
	}

	if charRange {
		charSet.Add('-')
	}

	if charSetInverted || charSetMayBeInverted {
		charSet.Invert()
	}

	return charSet, bytesLeft
}

const u32Max = ^uint32(0)

var all charset = [...]uint32{u32Max, u32Max, u32Max, u32Max, u32Max, u32Max, u32Max, u32Max}

func (set *MatchSet) readAtom(bytes []byte) (rune, []byte) {
	if bytes[0] == '[' {
		charSet, bytesLeft := charSetFromGroup(bytes[1:])
		if atom, ok := set.charSetToAtom[charSet]; ok {
			return atom, bytesLeft
		}

		if charSet == all {
			return magicUnderscore, bytesLeft
		}

		if set.nextAtom == 0 {
			set.nextAtom = 256
			set.charSetToAtom = make(map[charset]rune)
			set.atomToCharSet = make(map[rune]charset)
		}

		atom := set.nextAtom
		set.nextAtom++

		set.charSetToAtom[charSet] = atom
		set.atomToCharSet[atom] = charSet

		return atom, bytesLeft
	}

	atom, bytesLeft := rune(bytes[0]), bytes[1:]

	switch atom {
	case '*':
		atom = magicAsterisk

		if len(bytesLeft) != 0 && bytesLeft[0] == '*' {
			atom = magicAsterisk2
			bytesLeft = bytesLeft[1:]
		}
	case '?':
		atom = magicQuestion
	case '_':
		atom = magicUnderscore
	}

	return atom, bytesLeft
}

func (set *MatchSet) parse(bytes []byte) []rune {
	var currentAtom, lastAtom rune

	atoms := set.atomsBuffer[:0]

	for len(bytes) != 0 {
		currentAtom, bytes = set.readAtom(bytes)
		switch currentAtom {
		case magicAsterisk:
			if lastAtom != magicAsterisk && lastAtom != magicAsterisk2 {
				atoms = append(atoms, magicAsterisk)
				lastAtom = magicAsterisk
			}
		case magicAsterisk2:
			switch {
			case lastAtom == magicAsterisk:
				atoms[len(atoms)-1] = magicAsterisk2
				lastAtom = magicAsterisk2
			case lastAtom != magicAsterisk2:
				atoms = append(atoms, magicAsterisk2)
				lastAtom = magicAsterisk2
			}
		case magicQuestion:
			if lastAtom == magicAsterisk {
				atoms[len(atoms)-1] = magicQuestion
				atoms = append(atoms, magicAsterisk)
			} else {
				atoms = append(atoms, magicQuestion)
				lastAtom = magicQuestion
			}
		case magicUnderscore:
			if lastAtom == magicAsterisk2 {
				atoms[len(atoms)-1] = magicUnderscore
				atoms = append(atoms, magicAsterisk2)
			} else {
				atoms = append(atoms, magicUnderscore)
				lastAtom = magicUnderscore
			}
		default:
			atoms = append(atoms, currentAtom)
			lastAtom = currentAtom
		}
	}

	set.atomsBuffer = atoms

	if len(atoms) == 0 {
		// Empty pattern matches any characters.
		return []rune{magicDot}
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

func (set *MatchSet) Add(patt string, data any) {
	atoms := set.parse([]byte(patt))

	// Reverse atoms for better performance, because in practice,
	// patterns like ".abc" are much more frequently used than
	// patterns like "abc.".
	{
		for i, j := 0, len(atoms)-1; i < j; i, j = i+1, j-1 {
			atoms[i], atoms[j] = atoms[j], atoms[i]
		}

		for i, j := 0, len(atoms)-1; i < j; i++ {
			if atoms[i] == magicAsterisk && atoms[i+1] == magicQuestion ||
				atoms[i] == magicAsterisk2 && atoms[i+1] == magicUnderscore {
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

func (set *MatchSet) Match(source string, accumulate func(any)) {
	if !set.Empty() {
		set.match(source, accumulate)
	}
}

func (set *MatchSet) match(source string, accumulate func(any)) {
	bytes := []byte(source)

	// Also reverse source bytes, since all patterns are reversed.
	for i, j := 0, len(bytes)-1; i < j; i, j = i+1, j-1 {
		bytes[i], bytes[j] = bytes[j], bytes[i]
	}

	var patterns, matches []pattern

	for _, patt := range set.patterns {
		patterns = patterns[:0]
		patterns = append(patterns, patt)

		for i := range bytes {
			matches = matches[:0]

			for _, patt := range patterns {
				matches = set.checkPattern(patt, bytes, i, matches, accumulate)
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
	accumulate func(any),
) []pattern {
	atom, size := utf8.DecodeRune(patt.atoms)
	nextpatt := pattern{patt.atoms[size:], patt.data}

	switch {
	case atom == magicDot:
		if len(nextpatt.atoms) == 0 {
			if i == 0 || bytes[i] == '.' {
				accumulate(patt.data)
			}

			return matches
		}

		if i == 0 {
			matches = set.checkPattern(nextpatt, bytes[i:], 0, matches, accumulate)
		}

		if bytes[i] == '.' {
			matches = append(matches, nextpatt)
		}

		matches = append(matches, patt)
	case atom == magicAsterisk || atom == magicAsterisk2:
		if len(nextpatt.atoms) != 0 {
			matches = set.checkPattern(nextpatt, bytes[i:], 0, matches, accumulate)
		}

		if atom == magicAsterisk {
			switch bytes[i] {
			case '.', ':':
				return matches // magicAsterisk does not match '.' or ':'.
			}
		}

		if i+1 == len(bytes) {
			if len(nextpatt.atoms) == 0 {
				accumulate(patt.data)
			}

			return matches
		}

		matches = append(matches, patt)
	case atom == magicQuestion || atom == magicUnderscore:
		if atom == magicQuestion {
			switch bytes[i] {
			case '.', ':':
				return matches // magicQuestion does not match '.' or ':'.
			}
		}

		fallthrough
	case atom == rune(bytes[i]) || set.atomToCharSet[atom].Contains(bytes[i]):
		if i+1 == len(bytes) {
			switch len(nextpatt.atoms) {
			case 0:
				accumulate(patt.data)
			case 1:
				switch nextpatt.atoms[0] {
				case magicDot, magicAsterisk, magicAsterisk2:
					accumulate(patt.data)
				}
			case 2:
				switch nextpatt.atoms[0] {
				case magicAsterisk, magicAsterisk2:
					if nextpatt.atoms[1] == magicDot {
						accumulate(patt.data)
					}
				}
			}

			return matches
		}

		if len(nextpatt.atoms) == 0 {
			return matches
		}

		matches = append(matches, nextpatt)
	}

	return matches
}

func (set *MatchSet) MatchAll(source string) (matches []any) {
	set.Match(source, func(data any) { matches = append(matches, data) })
	return
}
