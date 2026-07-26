package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mehrnet/bgp-api/internal/api"
)

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Fatalf("parse DATABASE_URL: %v", err)
	}
	poolConfig.MaxConns = int32(environmentInt("POSTGRES_MAX_CONNECTIONS", 8))
	poolConfig.MinConns = 1
	poolConfig.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		log.Fatalf("connect PostgreSQL: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(context.Background()); err != nil {
		log.Fatalf("ping PostgreSQL: %v", err)
	}

	handler := api.New(api.NewPostgresRepository(pool), api.Config{
		AllowedOrigins:  allowedOrigins(os.Getenv("CORS_ALLOWED_ORIGINS_JSON")),
		OriginAuthToken: os.Getenv("ORIGIN_AUTH_TOKEN"),
		DatabaseEngine:  "postgresql",
	})
	server := &http.Server{
		Addr:              listenAddress(),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("bgp-api listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func listenAddress() string {
	if address := os.Getenv("LISTEN_ADDR"); address != "" {
		return address
	}
	return "127.0.0.1:" + strconv.Itoa(environmentInt("PORT", 3102))
}

func environmentInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		log.Fatalf("%s must be a positive integer", name)
	}
	return parsed
}

func allowedOrigins(value string) map[string]struct{} {
	if value == "" {
		return map[string]struct{}{}
	}
	var origins []string
	if err := json.Unmarshal([]byte(value), &origins); err != nil {
		log.Fatalf("CORS_ALLOWED_ORIGINS_JSON must be a JSON array: %v", err)
	}
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		origin = strings.TrimSpace(origin)
		if origin != "" {
			allowed[origin] = struct{}{}
		}
	}
	return allowed
}
