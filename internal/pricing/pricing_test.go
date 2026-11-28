package pricing

import (
	"math"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestTheBuiltInTableParsesAndCarriesItsDate(t *testing.T) {
	tab, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if tab.Taken != "2026-11-03" || tab.Source == "" || len(tab.Models) != 4 {
		t.Errorf("table = %+v", tab)
	}
}

func TestCostFollowsTheRatesAndTheCacheWriteLifetimes(t *testing.T) {
	tab, _ := Default()
	cases := []struct {
		name  string
		model string
		r     Request
		want  float64
	}{
		{"opus: input, read, output", "claude-opus-5-5", Request{Input: 1e6, CacheRead: 1e6, Output: 1e6}, 4 + 0.20 + 20},
		{"sonnet 5m write is input x 1.25", "claude-sonnet-5-5", Request{CacheWrite5m: 1e6}, 2.5},
		{"sonnet 1h write is input x 2", "claude-sonnet-5-5", Request{CacheWrite1h: 1e6}, 4},
		{"fable", "claude-fable-5-2", Request{Input: 1e6, CacheRead: 1e6, Output: 1e6}, 8 + 0.20 + 40},
		{"haiku at the edge of 100K is the low tier", "claude-haiku-5-5", Request{Input: 100000, Output: 1e6}, 0.01 + 0.50},
		{"haiku one token past 100K is the high tier", "claude-haiku-5-5", Request{Input: 99999, CacheRead: 2, Output: 1e6}, 99999*0.5/1e6 + 2*0.05/1e6 + 2.5},
		{"haiku prompt counts cache reads and writes", "claude-haiku-5-5", Request{CacheWrite5m: 50000, CacheRead: 60000}, 50000*0.5*1.25/1e6 + 60000*0.05/1e6},
		{"a dated release uses the listed model", "claude-opus-5-5-20261101", Request{Output: 1e6}, 20},
		{"no tokens costs nothing under any model", "<synthetic>", Request{}, 0},
	}
	for _, c := range cases {
		got, ok := tab.Cost(c.model, c.r)
		if !ok || !near(got, c.want) {
			t.Errorf("%s: %v %v, want %v", c.name, got, ok, c.want)
		}
	}
	if _, ok := tab.Cost("claude-mystery-9", Request{Output: 5}); ok {
		t.Error("an unknown model was priced")
	}
	if _, ok := tab.Cost("claude-opus-5-55", Request{Output: 5}); ok {
		t.Error("a name that only shares a prefix was priced")
	}
}

func TestAnInvalidPriceFileIsRefused(t *testing.T) {
	for name, data := range map[string]string{
		"no date":      "source: x\ncache_write: {five_minute: 1.25, one_hour: 2}\nmodels: {a: {input: 1, output: 1}}",
		"last tier":    "taken: d\nsource: x\ncache_write: {five_minute: 1.25, one_hour: 2}\nmodels:\n  a:\n    tiers:\n      - {up_to: 5, input: 1, output: 1}",
		"no multiples": "taken: d\nsource: x\nmodels: {a: {input: 1, output: 1}}",
		"no rates":     "taken: d\nsource: x\ncache_write: {five_minute: 1.25, one_hour: 2}\nmodels: {a: {}}",
	} {
		if _, err := Parse([]byte(data)); err == nil || !strings.Contains(err.Error(), "prices") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
