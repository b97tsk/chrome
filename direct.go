package main

import (
	"net"
	"time"
)

var direct = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}
