package service

import (
	"fmt"
	"log"
)

func writeLog(a ...interface{}) {
	log.Print("[service] ", fmt.Sprint(a...))
}

func writeLogf(format string, a ...interface{}) {
	log.Print("[service] ", fmt.Sprintf(format, a...))
}
