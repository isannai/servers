package main

// cmd/market — the asset registry backend (sibling of cmd/gate / cmd/rendezvous).
// Stores signed install.ian recipes + metadata; serves public browse/search
// and author-signed publish. See docs/TODO/market/market-server.md.

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/isannai/isann-servers/pkg/glog"
	"github.com/isannai/isann-servers/pkg/market"
)

func main() {
	configPath := flag.String("config", "conf/market.json", "path to config file")
	flag.Parse()

	cfg, err := market.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	glog.New(glog.Config{
		Output:   "file",
		File:     "logs/market.log",
		Rotate:   "daily",
		MaxFiles: 14,
	})
	log.Printf("[market] loaded config from %s", *configPath)

	store, err := market.NewStore(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		log.Fatalf("failed to open store (%s): %v", cfg.DB.Driver, err)
	}
	defer store.Close()
	log.Printf("[market] database ready (%s: %s)", cfg.DB.Driver, cfg.DB.DSN)

	srv := market.NewServer(cfg, store)

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		log.Println("[market] shutting down...")
		store.Close()
		os.Exit(0)
	}()

	if err := srv.Run(); err != nil {
		log.Fatalf("[market] server error: %v", err)
	}
}
