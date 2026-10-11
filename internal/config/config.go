// Package config reads blygd's settings from the environment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/chrisbodhi/blygd/internal/blyg"
)

// Version is blygd's own version, reported as the manifest generator.
const Version = "0.1.0-dev"

// Config is the process configuration.
type Config struct {
	DatabaseURL string // BLYGD_DATABASE_URL (required)
	Listen      string // BLYGD_LISTEN, default :8080
	// PublicURL is where origins mount: https://host/ gives
	// https://host/{handle}/. BLYGD_PUBLIC_URL (required).
	PublicURL string
	// TrustProxy honors X-Forwarded-Proto, and charges request budgets to
	// the last X-Forwarded-For address (BLYGD_TRUST_PROXY). Set it only
	// behind a proxy that appends that header, or anyone can pick the
	// address they are charged to.
	TrustProxy   bool
	AllowPrivate bool   // BLYGD_ALLOW_PRIVATE_FETCH: dev only
	GeneratorURL string // BLYGD_GENERATOR_URL
	FragmentCap  int    // BLYGD_FRAGMENT_CAP, default 2000 (§5.3)
	MediaMax     int    // BLYGD_MEDIA_MAX_BYTES, default 10 MiB
	// PollFloor is the least seconds between user-asked polls of one origin
	// (BLYGD_MANUAL_POLL_FLOOR, default 60). Lower it for development only:
	// the mirror is shared, and the floor is what keeps one tenant from
	// hammering an origin.
	PollFloor int
	Level     int // BLYGD_LEVEL, default 2
	// MigrateOnStart applies pending migrations before any command
	// (BLYGD_MIGRATE_ON_START, default true). Off, an out-of-date schema is
	// refused and `blygd migrate` is the only way forward.
	MigrateOnStart bool
	// RateLimits turns the request budgets on (BLYGD_RATE_LIMITS, default
	// true). Off is for test harnesses only (scripts/interop).
	RateLimits bool
}

// Load reads the environment.
func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:    os.Getenv("BLYGD_DATABASE_URL"),
		Listen:         envOr("BLYGD_LISTEN", ":8080"),
		PublicURL:      os.Getenv("BLYGD_PUBLIC_URL"),
		TrustProxy:     os.Getenv("BLYGD_TRUST_PROXY") == "true",
		AllowPrivate:   os.Getenv("BLYGD_ALLOW_PRIVATE_FETCH") == "true",
		GeneratorURL:   envOr("BLYGD_GENERATOR_URL", "https://github.com/chrisbodhi/blygd"),
		MigrateOnStart: os.Getenv("BLYGD_MIGRATE_ON_START") != "false",
		RateLimits:     os.Getenv("BLYGD_RATE_LIMITS") != "false",
	}
	var err error
	if c.FragmentCap, err = intEnv("BLYGD_FRAGMENT_CAP", 2000); err != nil {
		return nil, err
	}
	if c.MediaMax, err = intEnv("BLYGD_MEDIA_MAX_BYTES", 10<<20); err != nil {
		return nil, err
	}
	if c.PollFloor, err = intEnv("BLYGD_MANUAL_POLL_FLOOR", 60); err != nil {
		return nil, err
	}
	if c.Level, err = intEnv("BLYGD_LEVEL", 2); err != nil {
		return nil, err
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("BLYGD_DATABASE_URL is required")
	}
	if c.PublicURL == "" {
		return nil, errors.New("BLYGD_PUBLIC_URL is required (e.g. https://blyg.example/)")
	}
	if c.PublicURL, err = blyg.NormalizeOrigin(c.PublicURL); err != nil {
		return nil, fmt.Errorf("BLYGD_PUBLIC_URL: %w", err)
	}
	return c, nil
}

// Scheme is the public scheme.
func (c *Config) Scheme() string {
	u, _ := url.Parse(c.PublicURL)
	return u.Scheme
}

// Generator is the manifest generator string, name/version (§3.2).
func (c *Config) Generator() string { return "blygd/" + Version }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func intEnv(k string, def int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}
