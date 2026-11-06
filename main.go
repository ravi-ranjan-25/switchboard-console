// Switchboard v2: an app console for the citadel GitOps repo. Every change is a git commit made through the
// GitHub App; the cluster is only read, except for asking Flux to fetch now.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadConfig(osGet, os.ReadFile)
	if err != nil {
		log.Error("config", "err", err.Error())
		os.Exit(2)
	}
	itr, err := ghinstallation.New(http.DefaultTransport, cfg.AppID, cfg.InstallationID, cfg.PrivateKey)
	if err != nil {
		log.Error("github app key", "err", err.Error())
		os.Exit(2)
	}
	if cfg.GitHubBaseURL != "" {
		itr.BaseURL = cfg.GitHubBaseURL
	}
	repo, err := NewGitHubRepo(&http.Client{Transport: itr, Timeout: 20 * time.Second}, cfg)
	if err != nil {
		log.Error("github", "err", err.Error())
		os.Exit(2)
	}
	if cfg.DevNoAuth {
		log.Warn("DEV_NO_AUTH=1: identity check disabled (local development only)")
	}
	var cl Cluster = noCluster{}
	if cfg.NoCluster && cfg.DemoCluster {
		log.Warn("DEV_DEMO_CLUSTER=1: serving canned fake cluster data")
		cl = newDemoCluster()
	}
	if !cfg.NoCluster {
		if cl, err = NewK8sCluster(cfg); err != nil {
			log.Error("kubernetes", "err", err.Error())
			os.Exit(2)
		}
	}
	srv, err := NewServer(cfg, repo, cl, log)
	if err != nil {
		log.Error("server", "err", err.Error())
		os.Exit(2)
	}
	if rc, err := NewRepoCreator(cfg); err != nil {
		log.Error("repo creator", "err", err.Error())
		os.Exit(2)
	} else if rc != nil {
		srv.SetCreator(rc)
		log.Info("create-new-project enabled", "owner", cfg.RepoOwner)
	}
	if cfg.VaultID != "" {
		v, err := NewOCIVault(cfg.CompartmentID, cfg.VaultID, cfg.KeyID)
		if err != nil {
			log.Error("vault", "err", err.Error())
			os.Exit(2)
		}
		srv.SetVault(v)
		log.Info("secrets screen enabled (app-* secrets only)")
	}
	if cfg.VaultID == "" && cfg.DemoCluster {
		srv.SetVault(newDemoVault())
		log.Warn("DEV_DEMO_CLUSTER=1: demo vault with fake values")
	}
	// Warm: read the apps at startup so a bad token/repo fails loudly in the log.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if _, err := srv.snapshot(ctx, true); err != nil {
		log.Error("initial app load failed", "err", err.Error())
	}
	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = hs.Shutdown(sc)
	}()
	log.Info("listening", "addr", cfg.Listen, "repo", cfg.Owner+"/"+cfg.Repo)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("serve", "err", err.Error())
		os.Exit(1)
	}
}
