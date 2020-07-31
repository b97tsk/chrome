package logging

import (
	"fmt"
	"log"
)

func writeLog(a ...interface{}) {
	log.Print("[logging] ", fmt.Sprint(a...))
}

func writeLogf(format string, a ...interface{}) {
	log.Print("[logging] ", fmt.Sprintf(format, a...))
}
