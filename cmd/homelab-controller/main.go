package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/asdf57/prov-controller-test/go/internal/api"
	"github.com/asdf57/prov-controller-test/go/internal/config"
	etcdstore "github.com/asdf57/prov-controller-test/go/internal/store/etcd"
	"go.etcd.io/etcd/api/v3/mvccpb"
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
	defer func(etcdClient *clientv3.Client) {
		err := etcdClient.Close()
		if err != nil {
			slog.Error("etcd client close", "error", err)
		}
	}(etcdClient)

	go func() {
		watchChan := etcdClient.Watch(context.Background(), configuration.EtcdPrefix, clientv3.WithPrefix(), clientv3.WithPrevKV())
		for watchResp := range watchChan {
			if err := watchResp.Err(); err != nil {
				log.Printf("Watch error: %v", err)
				continue
			}

			for _, event := range watchResp.Events {
				switch event.Type {
				case mvccpb.PUT:
					slog.Info("PUT event", "key", string(event.Kv.Key), "value", event.Kv.Value)
					if event.PrevKv != nil {
						slog.Info("Prev val", "value", event.PrevKv.Value)
					}
				case mvccpb.DELETE:
					slog.Info("DELETE event", "key", string(event.Kv.Key))
					if event.PrevKv != nil {
						slog.Info("Prev val", "value", event.PrevKv.Value)
					}

				}
			}
		}
	}()

	resourceStore := etcdstore.New(etcdClient, configuration.EtcdPrefix)

	// Controller subsystem wiring is intentionally disabled until Store.Watch
	// and Manager.Serve are implemented.
	//
	// controllerStore := controller.NewControllerStore(resourceStore)

	handler := api.New(logger, resourceStore, configuration.RequestTimeout)
	server := &http.Server{
		Addr:              configuration.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Top level context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// machineReportController := machine.NewMachineReportController()
	// manager := controller.NewManager(
	// 	controllerStore,
	// 	[]controller.Registration{
	// 		{
	// 			Name:           "machine-report-controller",
	// 			ReconcilesKind: "MachineReport",
	// 			Watches:        []controller.Watch{{Kind: "MachineReport"}},
	// 			ResyncPeriod:   5 * time.Minute,
	// 			Reconciler:     machineReportController,
	// 		},
	// 	},
	// )
	//
	// controllerError := make(chan error, 1)
	// go func() {
	// 	logger.Info("starting controller subsystem")
	// 	controllerError <- manager.Serve(ctx)
	// }()

	// Create a channel for the HTTP server (so we can exit if/when it fails)
	serverError := make(chan error, 1)
	go func() {
		logger.Info("HTTP server starting", "address", configuration.HTTPAddr, "etcdEndpoints", configuration.EtcdEndpoints)
		serverError <- server.ListenAndServe()
	}()

	// This is where the main go routine will hang for the majority of the API's lifetime
	select {
	case err := <-serverError:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown requested")
		// Enable this case with the controller subsystem wiring above.
		// case err := <-controllerError:
		// 	return err
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
