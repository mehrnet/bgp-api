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
	if environmentBool("BGP_API_PRELOAD_COMPACT_SELECTORS", false) {
		if err := preloadCompactSelectors(store); err != nil {
			log.Fatalf("preload compact selectors: %v", err)
		}
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

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	for received := range signals {
		if received == syscall.SIGUSR1 {
			go func() {
				if err := preloadCompactSelectors(store); err != nil {
					log.Printf("preload compact selectors: %v", err)
				}
			}()
			continue
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("shutdown: %v", err)
		}
		cancel()
		return
	}
}

func preloadCompactSelectors(store *api.BboltRepository) error {
	started := time.Now()
	preload, err := store.PreloadCompactSelectors()
	if err != nil {
		return err
	}
	log.Printf("preloaded %.1f MiB of compact IP selectors in %s", float64(preload.Bytes)/(1<<20), time.Since(started).Round(time.Millisecond))
	return nil
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

func environmentBool(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Fatalf("%s must be a boolean", name)
		return false
	}
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
