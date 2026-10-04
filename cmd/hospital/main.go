package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hospital/internal/api"
	"hospital/internal/config"
	"hospital/internal/notify"
	"hospital/internal/remedy"
	"hospital/internal/runbook"
	"hospital/internal/store"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	var logLevel slog.Level
	v := os.Getenv("LOG_LEVEL")
	if v == "" {
		v = "info"
	}
	switch v {
	case "debug":
		logLevel = slog.LevelDebug
	case "info":
		logLevel = slog.LevelInfo
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	if err := run(log); err != nil {
		log.Error("hospital stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := openStore(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := runbook.LoadFile(ctx, st, cfg.RunbooksFile); err != nil {
		return err
	}

	client, err := kubeClient()
	if err != nil {
		log.Warn("kubernetes client unavailable; API actions will fail until a cluster config is present", "error", err)
		// return err
	}
	exec := remedy.NewKubernetesExecutor(client, remedy.NewPolicy(cfg.AllowedNamespaces, cfg.AllowSystemNamespaces), cfg.DryRun)
	slack := notify.NewSlack(cfg.SlackWebhook)
	worker := remedy.NewWorker(st, exec, slack, cfg.WorkerInterval, cfg.StaleAfter, log)
	go worker.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.New(st, slack, cfg.Token, cfg.PollTimeout, cfg.PollInterval, log).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("hospital listening", "addr", cfg.Addr, "dry_run", cfg.DryRun, "postgres", cfg.DatabaseURL != "")
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func openStore(ctx context.Context, dsn string) (store.Store, error) {
	if dsn == "" {
		return store.NewMemory(), nil
	}
	return store.OpenPostgres(ctx, dsn)
}

func kubeClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		loading := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		)
		cfg, err = loading.ClientConfig()
		if err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(cfg)
}
