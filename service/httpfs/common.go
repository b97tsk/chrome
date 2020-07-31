package httpfs

import (
	"fmt"
	"log"
)

func writeLog(a ...interface{}) {
	log.Print("[httpfs] ", fmt.Sprint(a...))
}

func writeLogf(format string, a ...interface{}) {
	log.Print("[httpfs] ", fmt.Sprintf(format, a...))
}
