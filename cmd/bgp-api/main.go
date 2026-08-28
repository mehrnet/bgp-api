package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
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
	databaseBytes, err := store.DatabaseSize()
	if err != nil {
		log.Fatalf("inspect bbolt dataset: %v", err)
	}
	selectorBytes, err := store.CompactSelectorBytes()
	if err != nil {
		log.Fatalf("inspect compact selectors: %v", err)
	}
	memoryTotal, err := hostMemoryTotal()
	if err != nil {
		log.Printf("could not inspect host memory; using the minimal cache strategy: %v", err)
	}
	cachePlan, err := resolveCachePlan(os.Getenv("BGP_API_CACHE_STRATEGY"), memoryTotal, databaseBytes, selectorBytes)
	if err != nil {
		log.Fatalf("select cache strategy: %v", err)
	}
	if cacheBytes, configured := environmentOptionalInt("BGP_API_COMPACT_CACHE_MIB"); configured {
		cachePlan.CompactCacheMiB = cacheBytes
	}
	if cacheBytes, configured := environmentOptionalInt("BGP_API_RESOURCE_CACHE_MIB"); configured {
		cachePlan.ResourceCacheMiB = cacheBytes
	}
	if cachePlan.Effective == "full" && memoryTotal < fullCacheRequirement(cachePlan) {
		log.Fatalf("select cache strategy: full cache strategy needs at least %.1f GiB after configured response-cache budgets; this host has %.1f GiB", float64(fullCacheRequirement(cachePlan))/float64(gibibyte), float64(memoryTotal)/float64(gibibyte))
	}
	deferCacheWarmup := environmentBool("BGP_API_DEFER_CACHE_WARMUP", false)
	runtimeCacheControl := environmentBool("BGP_API_RUNTIME_CACHE_CONTROL", false)
	log.Printf("cache strategy requested=%s effective=%s memory=%.1f GiB database=%.1f GiB selectors=%.1f MiB compact_cache=%d MiB resource_cache=%d MiB deferred=%t runtime_cache_control=%t", cachePlan.Requested, cachePlan.Effective, float64(memoryTotal)/float64(gibibyte), float64(databaseBytes)/float64(gibibyte), float64(selectorBytes)/float64(mebibyte), cachePlan.CompactCacheMiB, cachePlan.ResourceCacheMiB, deferCacheWarmup, runtimeCacheControl)

	config := api.Config{
		AllowedOrigins:  allowedOrigins(os.Getenv("CORS_ALLOWED_ORIGINS_JSON")),
		OriginAuthToken: os.Getenv("ORIGIN_AUTH_TOKEN"),
		Build: api.BuildInfo{
			Version: stringPointer(version),
			Commit:  stringPointer(commit),
			BuiltAt: stringPointer(builtAt),
		},
		RuntimeCacheControl:        runtimeCacheControl,
		CompactResponseCacheBytes:  cachePlan.CompactCacheMiB << 20,
		ResourceResponseCacheBytes: cachePlan.ResourceCacheMiB << 20,
	}
	warmer := runtimeCacheWarmer{store: store, plan: cachePlan}
	handler, runtimeCaches := api.NewWithRuntime(store, config)
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
	if deferCacheWarmup {
		log.Printf("cache warmup deferred until SIGUSR1")
	} else {
		warmer.Start()
	}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	for received := range signals {
		if received == syscall.SIGUSR1 {
			runtimeCaches.Enable()
			warmer.Start()
			continue
		}
		if received == syscall.SIGUSR2 {
			if runtimeCacheControl {
				releaseRuntimeCaches(store, runtimeCaches)
			} else {
				log.Printf("ignoring SIGUSR2 because BGP_API_RUNTIME_CACHE_CONTROL is disabled")
			}
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

func releaseRuntimeCaches(store *api.BboltRepository, caches *api.RuntimeCacheController) {
	selectorRelease := store.DropCompactSelectors()
	cacheRelease := caches.DisableAndClear()
	log.Printf("released runtime caches: selectors=%.1f MiB compact_entries=%d compact=%.1f MiB resource_entries=%d resource=%.1f MiB", float64(selectorRelease.Bytes)/(1<<20), cacheRelease.CompactEntries, float64(cacheRelease.CompactBytes)/(1<<20), cacheRelease.ResourceEntries, float64(cacheRelease.ResourceBytes)/(1<<20))
	go func() {
		runtime.GC()
		debug.FreeOSMemory()
		log.Printf("requested runtime cache memory return to the operating system")
	}()
}

type runtimeCacheWarmer struct {
	store   *api.BboltRepository
	plan    cachePlan
	mu      sync.Mutex
	running bool
}

func (warmer *runtimeCacheWarmer) Start() {
	warmer.mu.Lock()
	if warmer.running {
		warmer.mu.Unlock()
		return
	}
	warmer.running = true
	warmer.mu.Unlock()
	go func() {
		defer func() {
			warmer.mu.Lock()
			warmer.running = false
			warmer.mu.Unlock()
		}()
		if warmer.plan.PreloadSelectors {
			if err := preloadCompactSelectors(warmer.store); err != nil {
				log.Printf("preload compact selectors: %v", err)
				return
			}
		}
		if warmer.plan.WarmDatasetPageCache {
			log.Printf("warming %.1f GiB bbolt dataset through the kernel page cache", float64(warmer.plan.DatabaseBytes)/float64(gibibyte))
			warmup, err := warmer.store.WarmDataset(context.Background())
			if err != nil {
				log.Printf("warm bbolt dataset: %v", err)
				return
			}
			if !warmup.AlreadyWarm {
				log.Printf("warmed %.1f GiB bbolt dataset through the kernel page cache", float64(warmup.Bytes)/float64(gibibyte))
			}
		}
	}()
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

func environmentOptionalInt(name string) (int, bool) {
	value := os.Getenv(name)
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		log.Fatalf("%s must be a positive integer", name)
	}
	return parsed, true
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
