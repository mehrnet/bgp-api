package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	mebibyte                 int64 = 1 << 20
	gibibyte                       = 1 << 30
	balancedSystemReserve    int64 = 512 * mebibyte
	fullDatasetSystemReserve int64 = 768 * mebibyte
)

type cachePlan struct {
	Requested            string
	Effective            string
	CompactCacheMiB      int
	ResourceCacheMiB     int
	PreloadSelectors     bool
	WarmDatasetPageCache bool
	MemoryTotalBytes     int64
	DatabaseBytes        int64
	SelectorBytes        int64
}

func hostMemoryTotal() (int64, error) {
	contents, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		kibibytes, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kibibytes <= 0 {
			return 0, fmt.Errorf("parse MemTotal")
		}
		return kibibytes << 10, nil
	}
	return 0, fmt.Errorf("MemTotal is missing from /proc/meminfo")
}

func resolveCachePlan(requested string, memoryTotal, databaseBytes, selectorBytes int64) (cachePlan, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		requested = "auto"
	}
	plan := cachePlan{
		Requested:        requested,
		MemoryTotalBytes: memoryTotal,
		DatabaseBytes:    databaseBytes,
		SelectorBytes:    selectorBytes,
	}
	fullPlan := func() cachePlan {
		plan.Effective = "full"
		plan.CompactCacheMiB = 128
		plan.ResourceCacheMiB = 32
		plan.PreloadSelectors = false
		plan.WarmDatasetPageCache = true
		return plan
	}
	balancedPlan := func() cachePlan {
		plan.Effective = "balanced"
		plan.CompactCacheMiB = 256
		plan.ResourceCacheMiB = 64
		plan.PreloadSelectors = true
		plan.WarmDatasetPageCache = false
		return plan
	}
	minimalPlan := func() cachePlan {
		plan.Effective = "minimal"
		plan.CompactCacheMiB = 64
		plan.ResourceCacheMiB = 16
		plan.PreloadSelectors = false
		plan.WarmDatasetPageCache = false
		return plan
	}

	switch requested {
	case "minimal":
		return minimalPlan(), nil
	case "balanced":
		return balancedPlan(), nil
	case "full":
		candidate := fullPlan()
		if memoryTotal < fullCacheRequirement(candidate) {
			return cachePlan{}, fmt.Errorf("full cache strategy needs at least %.1f GiB for a %.1f GiB dataset; this host has %.1f GiB", float64(fullCacheRequirement(candidate))/float64(gibibyte), float64(databaseBytes)/float64(gibibyte), float64(memoryTotal)/float64(gibibyte))
		}
		return candidate, nil
	case "auto":
		candidate := fullPlan()
		if memoryTotal >= fullCacheRequirement(candidate) {
			return candidate, nil
		}
		candidate = balancedPlan()
		if memoryTotal >= minimumBalancedRequirement(candidate) {
			return candidate, nil
		}
		return minimalPlan(), nil
	default:
		return cachePlan{}, fmt.Errorf("BGP_API_CACHE_STRATEGY must be auto, minimal, balanced, or full")
	}
}

func fullCacheRequirement(plan cachePlan) int64 {
	return plan.DatabaseBytes + int64(plan.CompactCacheMiB+plan.ResourceCacheMiB)*mebibyte + fullDatasetSystemReserve
}

func minimumBalancedRequirement(plan cachePlan) int64 {
	return plan.SelectorBytes + int64(plan.CompactCacheMiB+plan.ResourceCacheMiB)*mebibyte + balancedSystemReserve
}
