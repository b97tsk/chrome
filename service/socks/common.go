package socks

import (
	"fmt"
	"log"
)

func writeLog(a ...interface{}) {
	log.Print("[socks] ", fmt.Sprint(a...))
}

func writeLogf(format string, a ...interface{}) {
	log.Print("[socks] ", fmt.Sprintf(format, a...))
}
