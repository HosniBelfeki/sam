package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/sam/internal/console"
	"github.com/google/sam/internal/secrets"
)

func main() {
	var (
		controlPlaneURL = flag.String("control-plane", "http://localhost:8080", "URL of the SAM control plane")
		adminTokenPath  = flag.String("admin-token-path", "", "Path to file containing the admin token (or env SAM_ADMIN_TOKEN)")
		bindAddr        = flag.String("bind-addr", ":8081", "Address to bind the console server")
		staticDir       = flag.String("static-dir", "public", "Directory containing static frontend files")
		basePath        = flag.String("base-path", "", "Base path prefix for the console (e.g. /console)")
	)
	flag.Parse()

	adminToken, err := secrets.FromPathOrEnv("admin-token", *adminTokenPath, "SAM_ADMIN_TOKEN")
	if err != nil {
		log.Fatalf("%v", err)
	}
	if adminToken == "" {
		log.Fatal("Admin token is required (via --admin-token-path or env SAM_ADMIN_TOKEN)")
	}

	srv, err := console.NewServer(console.Config{
		ControlPlaneURL: *controlPlaneURL,
		AdminToken:      adminToken,
		StaticDir:       *staticDir,
		BasePath:        *basePath,
	})
	if err != nil {
		log.Fatalf("Failed to initialize console server: %v", err)
	}

	httpSrv := &http.Server{
		Addr:    *bindAddr,
		Handler: srv.Handler(),
		// Mitigate Slowloris-style resource exhaustion from slow/malicious clients.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("SAM Console listening on %s", *bindAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Console server error: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	log.Println("Shutting down console server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}
}
