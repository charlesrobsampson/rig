// Package pricing turns a model's token counts into dollars with a table kept in
// prices.yaml (embedded), so rates can change without a code change.
package pricing

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed prices.yaml
var defaultYAML []byte

// Rate is USD per million tokens.
type Rate struct {
	Input     float64 `yaml:"input"`
	CacheRead float64 `yaml:"cache_read"`
	Output    float64 `yaml:"output"`
}

// Tier is a rate for prompts of up to UpTo tokens; the last tier has no UpTo and
// covers everything beyond.
type Tier struct {
	UpTo int64 `yaml:"up_to"`
	Rate `yaml:",inline"`
}

// Model is one model's rate, flat or in tiers by prompt size.
type Model struct {
	Rate  `yaml:",inline"`
	Tiers []Tier `yaml:"tiers"`
}

// Table is the parsed price file.
type Table struct {
	Taken      string `yaml:"taken"`
	Source     string `yaml:"source"`
	CacheWrite struct {
		FiveMinute float64 `yaml:"five_minute"`
		OneHour    float64 `yaml:"one_hour"`
	} `yaml:"cache_write"`
	Models map[string]Model `yaml:"models"`
}

// Request is what one model request used. CacheWrite5m and CacheWrite1h are the cache
// writes by time to live.
type Request struct {
	Input, CacheWrite5m, CacheWrite1h, CacheRead, Output int64
}

// Prompt is everything the request read.
func (r Request) Prompt() int64 { return r.Input + r.CacheWrite5m + r.CacheWrite1h + r.CacheRead }

// Parse reads a price file and checks it.
func Parse(data []byte) (*Table, error) {
	var t Table
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("prices: %w", err)
	}
	if t.Taken == "" || t.Source == "" {
		return nil, fmt.Errorf("prices: taken and source are required")
	}
	if t.CacheWrite.FiveMinute <= 0 || t.CacheWrite.OneHour <= 0 {
		return nil, fmt.Errorf("prices: cache_write multipliers are required")
	}
	for name, m := range t.Models {
		for i, tier := range m.Tiers {
			last := i == len(m.Tiers)-1
			switch {
			case last && tier.UpTo != 0:
				return nil, fmt.Errorf("prices: %s: the last tier must have no up_to", name)
			case !last && (tier.UpTo <= 0 || (i > 0 && tier.UpTo <= m.Tiers[i-1].UpTo)):
				return nil, fmt.Errorf("prices: %s: tiers need ascending up_to", name)
			}
		}
		if len(m.Tiers) == 0 && m.Input == 0 && m.Output == 0 {
			return nil, fmt.Errorf("prices: %s has no rates", name)
		}
	}
	return &t, nil
}

var (
	once   sync.Once
	def    *Table
	defErr error
)

// Default is the table built into rig.
func Default() (*Table, error) {
	once.Do(func() { def, defErr = Parse(defaultYAML) })
	return def, defErr
}

// Lookup finds a model by exact name, else by the longest listed name the model's name
// continues with a dash (a dated release of a listed model).
func (t *Table) Lookup(model string) (Model, bool) {
	if m, ok := t.Models[model]; ok {
		return m, true
	}
	var best string
	for name := range t.Models {
		if strings.HasPrefix(model, name+"-") && len(name) > len(best) {
			best = name
		}
	}
	m, ok := t.Models[best]
	return m, ok && best != ""
}

// Cost is the request's price in USD. ok is false for a model not in the table (the
// cost is then 0 and means nothing). A request that used no tokens costs nothing under
// any model (Claude Code writes such lines for its own messages).
func (t *Table) Cost(model string, r Request) (usd float64, ok bool) {
	if r == (Request{}) {
		return 0, true
	}
	m, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	rate := m.Rate
	if len(m.Tiers) > 0 {
		p := r.Prompt()
		for _, tier := range m.Tiers {
			rate = tier.Rate
			if tier.UpTo == 0 || p <= tier.UpTo {
				break
			}
		}
	}
	usd = (float64(r.Input)*rate.Input +
		float64(r.CacheWrite5m)*rate.Input*t.CacheWrite.FiveMinute +
		float64(r.CacheWrite1h)*rate.Input*t.CacheWrite.OneHour +
		float64(r.CacheRead)*rate.CacheRead +
		float64(r.Output)*rate.Output) / 1e6
	return usd, true
}

// Names lists the models in the table, sorted.
func (t *Table) Names() []string {
	var n []string
	for name := range t.Models {
		n = append(n, name)
	}
	sort.Strings(n)
	return n
}
