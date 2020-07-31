package pprof

import (
	"fmt"
	"log"
)

func writeLog(a ...interface{}) {
	log.Print("[pprof] ", fmt.Sprint(a...))
}

func writeLogf(format string, a ...interface{}) {
	log.Print("[pprof] ", fmt.Sprintf(format, a...))
}
