package main

import "testing"

func TestResolveCachePlan(t *testing.T) {
	database := int64(3)*gibibyte + 604*mebibyte
	selectors := int64(508 * mebibyte)
	cases := []struct {
		name      string
		requested string
		memory    int64
		want      string
		selectors bool
		pageCache bool
		wantError bool
	}{
		{name: "small auto", requested: "auto", memory: gibibyte, want: "minimal"},
		{name: "two gibibytes auto", requested: "auto", memory: 2 * gibibyte, want: "balanced", selectors: true},
		{name: "four gibibytes auto", requested: "auto", memory: 4 * gibibyte, want: "balanced", selectors: true},
		{name: "large auto", requested: "auto", memory: 6 * gibibyte, want: "full", pageCache: true},
		{name: "full on four gibibytes", requested: "full", memory: 4 * gibibyte, wantError: true},
		{name: "explicit minimal", requested: "minimal", memory: 8 * gibibyte, want: "minimal"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			plan, err := resolveCachePlan(test.requested, test.memory, database, selectors)
			if test.wantError {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.Effective != test.want {
				t.Fatalf("effective strategy = %q, want %q", plan.Effective, test.want)
			}
			if plan.PreloadSelectors != test.selectors || plan.WarmDatasetPageCache != test.pageCache {
				t.Fatalf("plan preload=%t page_cache=%t, want preload=%t page_cache=%t", plan.PreloadSelectors, plan.WarmDatasetPageCache, test.selectors, test.pageCache)
			}
		})
	}
}
