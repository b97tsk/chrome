package netiputil

import (
	"fmt"
	"net/netip"
)

func ParsePrefix(s string) (r AddrRange, closed bool, err error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		err = fmt.Errorf("parse prefix %q: %w", s, err)
		return
	}

	p = p.Masked()

	switch p.Addr().BitLen() {
	case 32:
		a4 := p.Addr().As4()
		bits := p.Bits()
		i0 := bits / 8

		for i := i0; i < len(a4); i++ {
			nz := 8
			if i == i0 {
				nz -= bits % 8
			}
			a4[i] |= byte(1<<nz - 1)
		}

		r = AddrRange{p.Addr(), netip.AddrFrom4(a4)}
		closed = true

		if ip := r.High.Next(); ip.IsValid() {
			r.High = ip
			closed = false
		}
	case 128:
		a16 := p.Addr().As16()
		bits := p.Bits()
		i0 := bits / 8

		for i := i0; i < len(a16); i++ {
			nz := 8
			if i == i0 {
				nz -= bits % 8
			}
			a16[i] |= byte(1<<nz - 1)
		}

		r = AddrRange{p.Addr(), netip.AddrFrom16(a16)}
		closed = true

		if ip := r.High.Next(); ip.IsValid() {
			r.High = ip
			closed = false
		}
	default:
		err = fmt.Errorf("parse prefix %q: unrecognized", s)
	}

	return
}
