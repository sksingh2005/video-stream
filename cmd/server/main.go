package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sksingh2005/video-stream/internal/api"
	"github.com/sksingh2005/video-stream/internal/config"
	"github.com/sksingh2005/video-stream/internal/storage"
	"github.com/sksingh2005/video-stream/internal/video"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	r2Client, err := storage.NewR2Client(cfg.R2)
	if err != nil {
		log.Fatalf("init r2 client: %v", err)
	}

	service := video.NewService(cfg, r2Client)
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	jobs := video.NewJobManager(cfg.Jobs, service)
	jobs.Start(workerCtx, cfg.Jobs.WorkerCount)
	server := &http.Server{
		Addr:              cfg.Server.Address,
		Handler:           api.NewHandler(service, jobs),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("video service listening on %s", cfg.Server.Address)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	workerCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
