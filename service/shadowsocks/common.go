package shadowsocks

import (
	"fmt"
	"log"
)

func writeLog(a ...interface{}) {
	log.Print("[shadowsocks] ", fmt.Sprint(a...))
}

func writeLogf(format string, a ...interface{}) {
	log.Print("[shadowsocks] ", fmt.Sprintf(format, a...))
}
