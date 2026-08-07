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

	"github.com/asdf57/prov-controller-test/go/internal/api"
	"github.com/asdf57/prov-controller-test/go/internal/config"
	etcdstore "github.com/asdf57/prov-controller-test/go/internal/store/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func main() {
	if err := run(); err != nil {
		slog.Error("control plane stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configuration, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: configuration.LogLevel}))
	slog.SetDefault(logger)

	etcdClient, err := clientv3.New(clientv3.Config{
		Endpoints:   configuration.EtcdEndpoints,
		DialTimeout: configuration.DialTimeout,
	})
	if err != nil {
		return err
	}
	defer etcdClient.Close()

	resourceStore := etcdstore.New(etcdClient, configuration.EtcdPrefix)
	handler := api.New(logger, resourceStore, configuration.RequestTimeout)
	server := &http.Server{
		Addr:              configuration.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverError := make(chan error, 1)
	go func() {
		logger.Info("HTTP server starting", "address", configuration.HTTPAddr, "etcdEndpoints", configuration.EtcdEndpoints)
		serverError <- server.ListenAndServe()
	}()

	select {
	case err := <-serverError:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown requested")
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), configuration.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		return err
	}
	if err := <-serverError; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
