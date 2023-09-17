package netiputil

import (
	"net/netip"
	"sort"
)

type AddrRange struct {
	Low, High netip.Addr
}

type AddrRangeSet []AddrRange

func (x *AddrRangeSet) Add(r AddrRange) {
	x.AddRange(r.Low, r.High)
}

func (x *AddrRangeSet) AddRange(lo, hi netip.Addr) {
	s := *x

	i := sort.Search(len(s), func(i int) bool { return s[i].Low.Compare(lo) > 0 })

	off := 0
	if i > 0 {
		off = i - 1
	}

	t := s[off:]
	j := off + sort.Search(len(t), func(i int) bool { return t[i].High.Compare(hi) > 0 })

	if i > j {
		return
	}

	if i > 0 {
		if r := &s[i-1]; r.High.Compare(lo) >= 0 {
			lo = r.Low
			i--
		}
	}

	if j < len(s) {
		if r := &s[j]; r.Low.Compare(hi) <= 0 {
			hi = r.High
			j++
		}
	}

	if i == j {
		if lo.Compare(hi) < 0 {
			s = append(s, AddrRange{})
			copy(s[i+1:], s[i:])
			s[i] = AddrRange{lo, hi}
			*x = s
		}

		return
	}

	s[i] = AddrRange{lo, hi}
	s = append(s[:i+1], s[j:]...)
	*x = s
}

func (x AddrRangeSet) Contains(v netip.Addr) bool {
	i := sort.Search(len(x), func(i int) bool { return x[i].High.Compare(v) > 0 })
	return i < len(x) && x[i].Low.Compare(v) <= 0
}
