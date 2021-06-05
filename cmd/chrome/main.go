package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/b97tsk/chrome"
	"github.com/b97tsk/chrome/service/http"
	"github.com/b97tsk/chrome/service/http/goagent"
	"github.com/b97tsk/chrome/service/http/httpfs"
	"github.com/b97tsk/chrome/service/http/pprof"
	"github.com/b97tsk/chrome/service/socks"
	"github.com/b97tsk/chrome/service/socks/shadowsocks"
	"github.com/b97tsk/chrome/service/socks/v2ray"
	"github.com/b97tsk/chrome/service/tcptun"
	"github.com/b97tsk/chrome/service/tcptun/dnstun"
	"github.com/fsnotify/fsnotify"
)

func main() {
	os.Exit(Main())
}

func Main() (code int) {
	flag.Parse()

	configFile := flag.Arg(0)
	if configFile == "" {
		base := filepath.Base(os.Args[0])
		ext := filepath.Ext(base)
		configFile = base[:len(base)-len(ext)] + ".yaml"

		if _, err := os.Stat(configFile); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if matches, _ := filepath.Glob("*.yaml"); len(matches) == 1 {
					configFile = matches[0]
					_, err = os.Stat(configFile)
				}
			}

			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
	}

	if configFile != "-" {
		abs, err := filepath.Abs(configFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}

		configFile = abs
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer watcher.Close()

	if configFile != "-" {
		err = watcher.Add(filepath.Dir(configFile))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	man := newManager()
	defer man.Shutdown()

	man.SetLogOutput(os.Stderr)

	if configFile == "-" {
		err = man.Load(os.Stdin)
	} else {
		err = man.LoadFile(configFile)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)

	defer signal.Stop(interrupt)

	var reload <-chan time.Time

	for {
		select {
		case e := <-watcher.Events:
			const Mask = fsnotify.Create | fsnotify.Rename | fsnotify.Write
			if e.Op&Mask != 0 && e.Name == configFile {
				reload = time.After(1 * time.Second)
			}
		case err := <-watcher.Errors:
			logger := man.Logger("main")
			logger.Warn(err)
		case <-reload:
			if err := man.LoadFile(configFile); err != nil {
				logger := man.Logger("main")
				logger.Warn(err)
			}

			reload = nil
		case <-interrupt:
			return
		}
	}
}

func newManager() *chrome.Manager {
	var m chrome.Manager

	m.AddService(dnstun.Service{})
	m.AddService(goagent.Service{})
	m.AddService(http.Service{})
	m.AddService(httpfs.Service{})
	m.AddService(pprof.Service{})
	m.AddService(shadowsocks.Service{})
	m.AddService(socks.Service{})
	m.AddService(tcptun.Service{})
	m.AddService(v2ray.Service{})

	return &m
}
