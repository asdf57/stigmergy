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
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/config"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/controller/inventory"
	"github.com/asdf57/prov-controller-test/go/internal/controller/machine"
	"github.com/asdf57/prov-controller-test/go/internal/controller/publication"
	servercontroller "github.com/asdf57/prov-controller-test/go/internal/controller/server"
	"github.com/asdf57/prov-controller-test/go/internal/controller/sshaccess"
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
	defer func(etcdClient *clientv3.Client) {
		err := etcdClient.Close()
		if err != nil {
			slog.Error("etcd client close", "error", err)
		}
	}(etcdClient)

	// Top level context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resourceStore := etcdstore.New(etcdClient, configuration.EtcdPrefix)

	handler := api.New(logger, resourceStore, configuration.RequestTimeout)
	server := &http.Server{
		Addr:              configuration.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	machineReportReconciler := machine.NewReconciler(resourceStore)
	machineReportController := controller.NewController(machineReportReconciler)
	serverReconciler := servercontroller.NewReconciler(resourceStore)
	serverController := controller.NewController(serverReconciler)
	inventoryReconciler := inventory.NewInventoryCaptureGroupReconciler(resourceStore)
	inventoryController := controller.NewController(inventoryReconciler)
	publicationReconciler := publication.NewReconciler(resourceStore)
	publicationController := controller.NewController(publicationReconciler)
	sshAccessReconciler := sshaccess.NewReconciler(resourceStore)
	sshAccessController := controller.NewController(sshAccessReconciler)
	inventoryWatches := []controller.Watch{
		{Kind: registry.InventoryCaptureGroupResource.Kind, Mapper: controller.IdentityMapper},
	}
	for _, definition := range inventory.CandidateDefinitions() {
		inventoryWatches = append(inventoryWatches, controller.Watch{Kind: definition.Kind, Mapper: inventoryReconciler.RequestsForResource})
	}

	manager := controller.NewManager(
		logger,
		resourceStore,
		[]controller.Registration{
			{
				Name:       "machine-report-controller",
				Controller: machineReportController,
				Watches: []controller.Watch{
					{Kind: "MachineReport", Mapper: controller.IdentityMapper},
				},
			},
			{
				Name:       "server-controller",
				Controller: serverController,
				Watches: []controller.Watch{
					{Kind: "Server", Mapper: controller.IdentityMapper},
					{Kind: "Machine", Mapper: serverReconciler.RequestsForMachine},
				},
			},
			{
				Name:       "inventory-controller",
				Controller: inventoryController,
				Watches:    inventoryWatches,
			},
			{
				Name:       "inventory-publication-controller",
				Controller: publicationController,
				Watches: []controller.Watch{
					{Kind: registry.InventoryPublicationResource.Kind, Mapper: controller.IdentityMapper},
					{Kind: registry.InventoryCaptureGroupResource.Kind, Mapper: publicationReconciler.RequestsForCaptureGroup},
					{Kind: registry.GitRepositoryResource.Kind, Mapper: publicationReconciler.RequestsForGitRepository},
				},
			},
			{
				Name:       "ssh-access-controller",
				Controller: sshAccessController,
				Watches: []controller.Watch{
					{Kind: registry.SSHAccessGrantResource.Kind, Mapper: controller.IdentityMapper},
					{Kind: registry.ServerResource.Kind, Mapper: sshAccessReconciler.RequestsForServer},
					{Kind: registry.SecretStoreResource.Kind, Mapper: sshAccessReconciler.RequestsForSecretStore},
				},
			},
		},
	)

	controllerError := make(chan error, 1)
	go func() {
		logger.Info("starting controller subsystem")
		controllerError <- manager.Serve(ctx)
	}()

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
