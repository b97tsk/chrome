package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/b97tsk/chrome/service"
	"github.com/b97tsk/chrome/service/http"
	"github.com/b97tsk/chrome/service/http/goagent"
	"github.com/b97tsk/chrome/service/http/httpfs"
	"github.com/b97tsk/chrome/service/pprof"
	"github.com/b97tsk/chrome/service/socks"
	"github.com/b97tsk/chrome/service/socks/shadowsocks"
	"github.com/b97tsk/chrome/service/socks/v2ray"
	"github.com/b97tsk/chrome/service/tcptun"
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
	}

	if configFile != "-" {
		abs, err := filepath.Abs(configFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}

		configFile = abs
	}

	configDir := filepath.Dir(configFile)
	os.Setenv("ConfigDir", configDir)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer watcher.Close()

	if configFile != "-" {
		err = watcher.Add(configDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	man := newManager()
	defer man.Shutdown()

	if configFile == "-" {
		man.Load(os.Stdin)
	} else {
		man.LoadFile(configFile)
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
			logger.WARN.Println(err)
		case <-reload:
			man.LoadFile(configFile)

			reload = nil
		case <-interrupt:
			return
		}
	}
}

func newManager() *service.Manager {
	man := service.NewManager()
	man.Add(goagent.Service{})
	man.Add(http.Service{})
	man.Add(httpfs.Service{})
	man.Add(pprof.Service{})
	man.Add(shadowsocks.Service{})
	man.Add(socks.Service{})
	man.Add(tcptun.Service{})
	man.Add(v2ray.Service{})

	return man
}
