package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/b97tsk/chrome/service"
	"github.com/b97tsk/chrome/service/goagent"
	"github.com/b97tsk/chrome/service/http"
	"github.com/b97tsk/chrome/service/httpfs"
	"github.com/b97tsk/chrome/service/logging"
	"github.com/b97tsk/chrome/service/pprof"
	"github.com/b97tsk/chrome/service/shadowsocks"
	"github.com/b97tsk/chrome/service/socks"
	"github.com/b97tsk/chrome/service/tcptun"
	"github.com/b97tsk/chrome/service/vmess"
	"github.com/fsnotify/fsnotify"
)

func main() {
	var configFile string
	flag.StringVar(&configFile, "conf", "chrome.yaml", "config file")
	flag.Parse()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Println("[watcher]", err)
		return
	}
	defer watcher.Close()

	err = watcher.Add(configFile)
	if err != nil {
		log.Println("[watcher]", err)
		return
	}

	os.Setenv("ConfigDir", filepath.Dir(configFile))

	services := service.NewManager()
	addServices(services)
	services.Load(configFile)
	defer services.Shutdown()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupt)

	var delay <-chan time.Time
	for {
		select {
		case e := <-watcher.Events:
			if e.Op&fsnotify.Write != 0 {
				delay = time.After(1 * time.Second)
			}
		case <-delay:
			services.Load(configFile)
			delay = nil
		case <-interrupt:
			return
		}
	}
}

func addServices(services *service.Manager) {
	services.Add(goagent.Service{})
	services.Add(http.Service{})
	services.Add(httpfs.Service{})
	services.Add(logging.Service{})
	services.Add(pprof.Service{})
	services.Add(shadowsocks.Service{})
	services.Add(socks.Service{})
	services.Add(tcptun.Service{})
	services.Add(vmess.Service{})
}
