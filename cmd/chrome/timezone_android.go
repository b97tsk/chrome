// https://github.com/golang/go/issues/20455#issuecomment-342287698

package main

import (
	"os/exec"
	"strings"
	"time"
)

func init() {
	if time.Local.String() != "UTC" {
		return
	}

	out, err := exec.Command("/system/bin/getprop", "persist.sys.timezone").Output()
	if err != nil {
		return
	}

	loc, err := time.LoadLocation(strings.TrimSpace(string(out)))
	if err != nil {
		return
	}

	time.Local = loc
}
