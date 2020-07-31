package v2ray

import (
	"fmt"
	"log"
)

func writeLog(a ...interface{}) {
	log.Print("[v2ray] ", fmt.Sprint(a...))
}

func writeLogf(format string, a ...interface{}) {
	log.Print("[v2ray] ", fmt.Sprintf(format, a...))
}
