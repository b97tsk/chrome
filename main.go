package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

func main() {
	flag.Parse()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Println("[watcher]", err)
		return
	}
	defer watcher.Close()

	err = watcher.Add(configFileName)
	if err != nil {
		log.Println("[watcher]", err)
		return
	}

	services.Load()
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
			services.Load()
			delay = nil
		case <-interrupt:
			return
		}
	}
}
