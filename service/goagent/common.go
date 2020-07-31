package goagent

import (
	"fmt"
	"log"
)

func writeLog(a ...interface{}) {
	log.Print("[goagent] ", fmt.Sprint(a...))
}

func writeLogf(format string, a ...interface{}) {
	log.Print("[goagent] ", fmt.Sprintf(format, a...))
}
