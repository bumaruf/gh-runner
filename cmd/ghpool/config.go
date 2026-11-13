package main

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bumaruf/gh-runner/internal/github"
	"github.com/bumaruf/gh-runner/internal/pool"
)

type config struct {
	APIURL         string
	Token          string
	AppID          string
	AppKey         *rsa.PrivateKey
	InstallationID int64
	DockerSocket   string
	LogLevel       slog.Level
	Pool           pool.Config
}

var poolNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// loadConfig reads the environment; README.md lists every variable.
func loadConfig(getenv func(string) string, readFile func(string) ([]byte, error)) (config, error) {
	env := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}
	var errs []error
	duration := func(key, fallback string) time.Duration {
		d, err := time.ParseDuration(env(key, fallback))
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: want a positive duration such as 5s", key))
		}
		return d
	}

	cfg := config{
		APIURL:       env("GITHUB_API_URL", "https://api.github.com"),
		Token:        env("GITHUB_TOKEN", ""),
		AppID:        env("GITHUB_APP_ID", ""),
		DockerSocket: env("DOCKER_SOCKET", "/var/run/docker.sock"),
	}

	switch {
	case cfg.AppID != "":
		keyFile := env("GITHUB_APP_PRIVATE_KEY_FILE", "")
		if keyFile == "" {
			errs = append(errs, errors.New("GITHUB_APP_PRIVATE_KEY_FILE is required with GITHUB_APP_ID"))
			break
		}
		pemData, err := readFile(keyFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE: %w", err))
			break
		}
		if cfg.AppKey, err = github.ParsePrivateKey(pemData); err != nil {
			errs = append(errs, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE: %w", err))
		}
		if v := env("GITHUB_APP_INSTALLATION_ID", ""); v != "" {
			if cfg.InstallationID, err = strconv.ParseInt(v, 10, 64); err != nil {
				errs = append(errs, errors.New("GITHUB_APP_INSTALLATION_ID: want a number"))
			}
		}
	case cfg.Token == "":
		errs = append(errs, errors.New("set GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY_FILE, or GITHUB_TOKEN"))
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	slots, err := strconv.Atoi(env("POOL_SIZE", "6"))
	if err != nil || slots < 1 || slots > 64 {
		errs = append(errs, errors.New("POOL_SIZE: want a number from 1 to 64"))
	}
	name := env("POOL_NAME", "ghpool")
	if !poolNamePattern.MatchString(name) {
		errs = append(errs, errors.New("POOL_NAME: want lowercase letters, digits and dashes, up to 31"))
	}
	cacheTTL := duration("CACHE_TTL", "336h")
	dockerInJobs, err := strconv.ParseBool(env("RUNNER_DOCKER", "false"))
	if err != nil {
		errs = append(errs, errors.New("RUNNER_DOCKER: want true or false"))
	}
	cpus, err := strconv.ParseFloat(env("RUNNER_CPUS", "0"), 64)
	if err != nil || cpus < 0 {
		errs = append(errs, errors.New("RUNNER_CPUS: want a number such as 4 or 2.5; 0 means no limit"))
	}
	memory, err := parseBytes(env("RUNNER_MEMORY", "0"))
	if err != nil {
		errs = append(errs, fmt.Errorf("RUNNER_MEMORY: %w", err))
	}

	cfg.Pool = pool.Config{
		Name:         name,
		Slots:        slots,
		Labels:       list(env("POOL_LABELS", "gh-runner")),
		Image:        env("RUNNER_IMAGE", "ghpool-runner:2.338.2"),
		CPUs:         cpus,
		Memory:       memory,
		Docker:       dockerInJobs,
		DindImage:    env("DIND_IMAGE", "docker:29.9.2-dind"),
		CacheTTL:     cacheTTL,
		PollInterval: duration("POLL_INTERVAL", "5s"),
		IdleTimeout:  duration("IDLE_TIMEOUT", "5m"),
		RepoRefresh:  duration("REPO_REFRESH", "5m"),
		Include:      list(env("REPOS_INCLUDE", "")),
		Exclude:      list(env("REPOS_EXCLUDE", "")),
		AllowPublic:  env("ALLOW_PUBLIC_REPOS", "false") == "true",
		AppAuth:      cfg.AppID != "",
	}
	return cfg, errors.Join(errs...)
}

func list(value string) []string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// parseBytes reads sizes such as 8g, 512m or a plain number of bytes.
func parseBytes(value string) (int64, error) {
	units := map[string]int64{"k": 1 << 10, "m": 1 << 20, "g": 1 << 30}
	value = strings.ToLower(value)
	multiplier := int64(1)
	if unit, ok := units[value[len(value)-1:]]; ok {
		multiplier, value = unit, value[:len(value)-1]
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("want a size such as 8g or 512m; 0 means no limit")
	}
	return n * multiplier, nil
}
