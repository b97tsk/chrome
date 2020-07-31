package http

import (
	"fmt"
	"log"
)

func writeLog(a ...interface{}) {
	log.Print("[http] ", fmt.Sprint(a...))
}

func writeLogf(format string, a ...interface{}) {
	log.Print("[http] ", fmt.Sprintf(format, a...))
}
