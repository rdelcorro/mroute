package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openclaw/mroute/internal/api"
	"github.com/openclaw/mroute/internal/config"
	"github.com/openclaw/mroute/internal/flow"
	"github.com/openclaw/mroute/internal/monitor"
	"github.com/openclaw/mroute/internal/telegram"
	"github.com/openclaw/mroute/internal/transport"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "config file path")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	bot := telegram.NewBot(cfg.Telegram.BotToken, cfg.Telegram.ChatID)
	engine := transport.NewEngine(cfg.FFmpeg.Path, cfg.FFmpeg.MaxFlows)
	engine.SetFFprobePath(cfg.FFmpeg.ProbePath)
	mon := monitor.NewMonitor(cfg.FFmpeg.Path, cfg.FFmpeg.ProbePath)

	mgr, err := flow.NewManager(cfg.Storage.DSN, engine, mon)
	if err != nil {
		log.Fatalf("flow manager: %v", err)
	}
	defer mgr.Close()

	// Wire telegram notifications to flow events
	mgr.SetEventCallback(func(flowID, eventType, message, severity string) {
		bot.NotifyFlowEvent(flowID, eventType, message, severity)
	})

	addr := fmt.Sprintf("%s:%d", cfg.API.Host, cfg.API.Port)
	srv := api.NewServer(addr, mgr)

	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: %v", err)
		}
	}()

	log.Printf("mroute started on %s", addr)
	bot.NotifyStartup(addr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("shutting down...")
	bot.NotifyShutdown()

	mon.StopAll()
	engine.StopAll()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	// Close telegram bot (drains pending messages)
	bot.Close()

	log.Println("mroute stopped")
}
