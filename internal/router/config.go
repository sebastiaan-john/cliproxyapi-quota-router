package router

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode selects how eligible credentials are ranked.
type Mode string

const (
	// ModeSmart ranks by usable quota divided by time until it resets.
	ModeSmart Mode = "smart"
	// ModeExpiringFirst is another name for ModeSmart. Ranking purely by the
	// earliest reset would prefer a nearly empty account over one that is about
	// to lose most of its quota, so the name maps to the quota-weighted ranking.
	ModeExpiringFirst Mode = "expiring-first"
	// ModeHeadroom ranks by the largest remaining share of the tightest window.
	ModeHeadroom Mode = "headroom"
)

// Fallback names the strategy used when quota data cannot rank candidates.
type Fallback string

const (
	FallbackRoundRobin Fallback = "round-robin"
	FallbackFillFirst  Fallback = "fill-first"
	// FallbackNone leaves the pick to the host's configured selector when no
	// candidate is excluded. When some are, the plugin picks round-robin itself
	// so the host cannot choose an excluded credential.
	FallbackNone Fallback = "none"
)

// Config is the plugin section under plugins.configs.<plugin-id>.
type Config struct {
	Mode            Mode    `yaml:"mode"`
	MinimumHeadroom float64 `yaml:"minimum_headroom"`
	// SessionMinimumHeadroom is the headroom a credential must keep to go on
	// serving a session already bound to it. It is at most MinimumHeadroom and
	// defaults to 0: a bound session stays until the quota is actually gone, so
	// the prompt cache built on that credential is not thrown away early.
	SessionMinimumHeadroom float64       `yaml:"session_minimum_headroom"`
	Fallback               Fallback      `yaml:"fallback"`
	RefreshInterval        time.Duration `yaml:"refresh_interval"`
	StaleAfter             time.Duration `yaml:"stale_after"`
	ProbeInterval          time.Duration `yaml:"probe_interval"`
	MinResetHorizon        time.Duration `yaml:"min_reset_horizon"`
	SwitchMargin           float64       `yaml:"switch_margin"`
	TieTolerance           float64       `yaml:"tie_tolerance"`
	SessionAffinity        bool          `yaml:"session_affinity"`
	SessionAffinityTTL     time.Duration `yaml:"session_affinity_ttl"`
	AcrossPriorities       bool          `yaml:"across_priorities"`
	LogDecisions           bool          `yaml:"log_decisions"`
	RevealAuthIDs          bool          `yaml:"reveal_auth_ids"`
	// PaidFallback allows credentials that bill per request (API keys, extra
	// usage) to be chosen when no subscription quota is usable. Off by default.
	PaidFallback bool `yaml:"paid_fallback"`
	// StateFile, when set, is an absolute path where session bindings are saved
	// so they survive a proxy restart. Empty disables persistence.
	StateFile string `yaml:"state_file"`
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		Mode:               ModeSmart,
		MinimumHeadroom:    0.02,
		Fallback:           FallbackRoundRobin,
		RefreshInterval:    5 * time.Minute,
		StaleAfter:         30 * time.Minute,
		ProbeInterval:      time.Minute,
		MinResetHorizon:    time.Hour,
		SwitchMargin:       0.15,
		TieTolerance:       0.05,
		SessionAffinity:    true,
		SessionAffinityTTL: time.Hour,
	}
}

// ParseConfig decodes the host-provided YAML over the defaults. Keys the host
// adds for itself (enabled, priority, store) are ignored.
func ParseConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode config: %w", err)
		}
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	c.Mode = Mode(strings.ToLower(strings.TrimSpace(string(c.Mode))))
	switch c.Mode {
	case "", ModeSmart, ModeExpiringFirst:
		c.Mode = ModeSmart
	case ModeHeadroom:
	default:
		return fmt.Errorf("mode %q: want smart, expiring-first or headroom", c.Mode)
	}
	c.Fallback = Fallback(strings.ToLower(strings.TrimSpace(string(c.Fallback))))
	switch c.Fallback {
	case "":
		c.Fallback = FallbackRoundRobin
	case FallbackRoundRobin, FallbackFillFirst, FallbackNone:
	default:
		return fmt.Errorf("fallback %q: want round-robin, fill-first or none", c.Fallback)
	}
	// NaN compares false against every bound, so it must be rejected explicitly.
	for name, v := range map[string]float64{
		"minimum_headroom":         c.MinimumHeadroom,
		"session_minimum_headroom": c.SessionMinimumHeadroom,
		"switch_margin":            c.SwitchMargin,
		"tie_tolerance":            c.TieTolerance,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s must be a finite number", name)
		}
	}
	if c.MinimumHeadroom < 0 || c.MinimumHeadroom >= 1 {
		return fmt.Errorf("minimum_headroom %v: want a fraction in [0, 1)", c.MinimumHeadroom)
	}
	if c.SessionMinimumHeadroom < 0 || c.SessionMinimumHeadroom > c.MinimumHeadroom {
		return fmt.Errorf("session_minimum_headroom %v: want a fraction in [0, minimum_headroom]", c.SessionMinimumHeadroom)
	}
	if c.SwitchMargin < 0 || c.TieTolerance < 0 || c.TieTolerance >= 1 {
		return fmt.Errorf("switch_margin and tie_tolerance must be non-negative fractions")
	}
	for name, d := range map[string]*time.Duration{
		"refresh_interval":     &c.RefreshInterval,
		"stale_after":          &c.StaleAfter,
		"probe_interval":       &c.ProbeInterval,
		"min_reset_horizon":    &c.MinResetHorizon,
		"session_affinity_ttl": &c.SessionAffinityTTL,
	} {
		if *d <= 0 {
			return fmt.Errorf("%s must be a positive duration such as \"5m\"", name)
		}
	}
	if c.RefreshInterval < 10*time.Second {
		c.RefreshInterval = 10 * time.Second
	}
	c.StateFile = strings.TrimSpace(c.StateFile)
	if c.StateFile != "" {
		if !filepath.IsAbs(c.StateFile) {
			return fmt.Errorf("state_file must be an absolute path")
		}
		c.StateFile = filepath.Clean(c.StateFile)
	}
	return nil
}

// ConfigView is the JSON form of Config used in diagnostics.
type ConfigView struct {
	Mode                   Mode     `json:"mode"`
	MinimumHeadroom        float64  `json:"minimum_headroom"`
	SessionMinimumHeadroom float64  `json:"session_minimum_headroom"`
	PaidFallback           bool     `json:"paid_fallback"`
	Fallback               Fallback `json:"fallback"`
	RefreshInterval        string   `json:"refresh_interval"`
	StaleAfter             string   `json:"stale_after"`
	ProbeInterval          string   `json:"probe_interval"`
	MinResetHorizon        string   `json:"min_reset_horizon"`
	SwitchMargin           float64  `json:"switch_margin"`
	TieTolerance           float64  `json:"tie_tolerance"`
	SessionAffinity        bool     `json:"session_affinity"`
	SessionAffinityTTL     string   `json:"session_affinity_ttl"`
	AcrossPriorities       bool     `json:"across_priorities"`
	LogDecisions           bool     `json:"log_decisions"`
	RevealAuthIDs          bool     `json:"reveal_auth_ids"`
	// StateFileEnabled reports persistence without revealing the path.
	StateFileEnabled bool `json:"state_file_enabled"`
}

// View returns the diagnostic form of c.
func (c Config) View() ConfigView {
	return ConfigView{
		Mode:                   c.Mode,
		MinimumHeadroom:        c.MinimumHeadroom,
		SessionMinimumHeadroom: c.SessionMinimumHeadroom,
		PaidFallback:           c.PaidFallback,
		Fallback:               c.Fallback,
		RefreshInterval:        c.RefreshInterval.String(),
		StaleAfter:             c.StaleAfter.String(),
		ProbeInterval:          c.ProbeInterval.String(),
		MinResetHorizon:        c.MinResetHorizon.String(),
		SwitchMargin:           c.SwitchMargin,
		TieTolerance:           c.TieTolerance,
		SessionAffinity:        c.SessionAffinity,
		SessionAffinityTTL:     c.SessionAffinityTTL.String(),
		AcrossPriorities:       c.AcrossPriorities,
		LogDecisions:           c.LogDecisions,
		RevealAuthIDs:          c.RevealAuthIDs,
		StateFileEnabled:       c.StateFile != "",
	}
}
