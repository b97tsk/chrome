package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

func main() {
	defer services.Shutdown()

	services.Load()

	waitWatcher := make(chan struct{})
	defer func() {
		// log.Println("[main] closing watcher")
		<-waitWatcher
	}()

	ctxWatcher, cancelWatcher := context.WithCancel(context.Background())
	defer cancelWatcher()

	go func() {
		defer close(waitWatcher)

		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			log.Println("[main]", err)
			return
		}
		defer watcher.Close()

		err = watcher.Add(configFileName)
		if err != nil {
			log.Println("[main]", err)
			return
		}

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
			case <-ctxWatcher.Done():
				return
			}
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	signal.Stop(c)
	// log.Println("[main] shuting down")
}
