package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/roccoren/ghcp-pool-go/internal/gateway"
)

func main() {
	settings, err := gateway.LoadSettings("")
	if err != nil {
		log.Fatalf("load settings: %v", err)
	}
	gw, err := gateway.NewGateway(settings)
	if err != nil {
		log.Fatalf("create gateway: %v", err)
	}
	if err := gw.Startup(context.Background()); err != nil {
		log.Fatalf("startup: %v", err)
	}
	defer gw.Shutdown()

	srv := &http.Server{
		Addr:              settings.Addr(),
		Handler:           gateway.NewServer(gw),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("ghcp-pool-go listening on %s", settings.Addr())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
