package main

import (
	"net"

	"github.com/b97tsk/chrome/configure"
)

var direct = &net.Dialer{
	Timeout:   configure.Timeout,
	KeepAlive: configure.KeepAlive,
}
