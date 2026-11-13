// Command ghpool keeps a pool of ephemeral self-hosted GitHub Actions runners on one Docker
// host and lends them to whichever private repo of the account has a job waiting.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bumaruf/gh-runner/internal/docker"
	"github.com/bumaruf/gh-runner/internal/github"
	"github.com/bumaruf/gh-runner/internal/pool"
)

func main() {
	cfg, err := loadConfig(os.Getenv, os.ReadFile)
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	var tokens github.TokenSource = github.StaticToken(cfg.Token)
	if cfg.AppID != "" {
		tokens = &github.AppTokenSource{
			AppID:          cfg.AppID,
			Key:            cfg.AppKey,
			InstallationID: cfg.InstallationID,
			BaseURL:        cfg.APIURL,
			HTTP:           &http.Client{Timeout: 30 * time.Second},
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p := pool.New(cfg.Pool, github.NewClient(cfg.APIURL, tokens), docker.New(cfg.DockerSocket), log)
	if err := p.Run(ctx); err != nil {
		log.Error("ghpool stopped", "err", err)
		os.Exit(1)
	}
}
