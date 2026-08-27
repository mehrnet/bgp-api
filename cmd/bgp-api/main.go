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

	"github.com/mehrnet/bgp-api/internal/api"
)

var (
	version = "dev"
	commit  = ""
	builtAt = ""
)

func main() {
	path := os.Getenv("BGP_API_DATABASE_PATH")
	if path == "" {
		path = "/var/lib/bgp-api/primary.bbolt"
	}
	store, err := api.NewBboltRepository(path)
	if err != nil {
		log.Fatalf("open bbolt dataset: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("close bbolt dataset: %v", err)
		}
	}()
	if os.Getenv("BGP_API_VALIDATE_ONLY") == "1" {
		metadata, err := store.DatasetMetadata(context.Background())
		if err != nil {
			log.Fatalf("validate bbolt dataset: %v", err)
		}
		releaseTag := "unknown"
		if metadata.ReleaseTag != nil {
			releaseTag = *metadata.ReleaseTag
		}
		log.Printf("validated bbolt dataset %s", releaseTag)
		return
	}

	config := api.Config{
		AllowedOrigins:  allowedOrigins(os.Getenv("CORS_ALLOWED_ORIGINS_JSON")),
		OriginAuthToken: os.Getenv("ORIGIN_AUTH_TOKEN"),
		Build: api.BuildInfo{
			Version: stringPointer(version),
			Commit:  stringPointer(commit),
			BuiltAt: stringPointer(builtAt),
		},
		CompactResponseCacheBytes:  environmentInt("BGP_API_COMPACT_CACHE_MIB", 256) << 20,
		ResourceResponseCacheBytes: environmentInt("BGP_API_RESOURCE_CACHE_MIB", 64) << 20,
	}
	handler := api.New(store, config)
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

func stringPointer(value string) *string {
	return &value
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
