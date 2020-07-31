package tcptun

import (
	"fmt"
	"log"
)

func writeLog(a ...interface{}) {
	log.Print("[tcptun] ", fmt.Sprint(a...))
}

func writeLogf(format string, a ...interface{}) {
	log.Print("[tcptun] ", fmt.Sprintf(format, a...))
}
