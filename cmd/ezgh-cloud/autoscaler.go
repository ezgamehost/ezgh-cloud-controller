package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/autoscaler"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/run"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
)

func init() { subcommands["autoscaler-provider"] = runAutoscalerProvider }

// runAutoscalerProvider serves Cluster Autoscaler's externalgrpc provider on 127.0.0.1:9447
// (plaintext, loopback only) on every control-plane VM: Cluster Autoscaler, leader-elected itself,
// calls its own VM's. The provider keeps no state, so it needs no Lease and no Kubernetes client.
// EZGH_CLOUD_CONFIG and EZGH_CREDENTIALS_URL override the VM's paths.
func runAutoscalerProvider([]string) error {
	log := run.Logger("autoscaler-provider")
	path := config.Path
	if p := os.Getenv("EZGH_CLOUD_CONFIG"); p != "" {
		path = p
	}
	p := &autoscaler.Provider{
		Clients:    ezghclient.New(&ezghclient.LocalSource{URL: os.Getenv("EZGH_CREDENTIALS_URL")}, "ezgh-cloud-autoscaler-provider"),
		ConfigPath: path,
	}
	lis, err := net.Listen("tcp", "127.0.0.1:9447")
	if err != nil {
		return err
	}
	// Failed calls are logged with their method and gRPC code, never their request.
	srv := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		resp, err := h(ctx, req)
		if err != nil {
			s := status.Convert(err)
			log.Warn("autoscaler call failed", "method", info.FullMethod, "code", s.Code().String(), "error", s.Message())
		}
		return resp, err
	}))
	autoscaler.RegisterCloudProviderServer(srv, p)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.GracefulStop() // running calls finish first
	}()
	log.Info("serving the Cluster Autoscaler provider", "addr", "127.0.0.1:9447")
	return srv.Serve(lis)
}
