package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	diffapi "github.com/containerd/containerd/api/services/diff/v1"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/contrib/diffservice"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	erofsdiff "github.com/containerd/containerd/v2/plugins/diff/erofs"
	snapshot "github.com/containerd/containerd/v2/plugins/snapshots/erofs"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
)

var (
	rootDir        = flag.String("root", "/var/lib/containerd-erofs/snapshotter", "EROFS snapshotter root directory")
	sockAddr       = flag.String("addr", "/run/containerd-erofs-grpc/containerd-erofs-grpc.sock", "Socket path to listen on")
	containerdAddr = flag.String("containerd-addr", "/run/containerd/containerd.sock", "Address for containerd's GRPC server")
)

func main() {
	flag.Parse()

	if err := serve(*containerdAddr, *sockAddr, *rootDir); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}

func serve(containerdAddress, address, root string) error {
	// Prepare the address directory
	if err := os.MkdirAll(filepath.Dir(address), 0700); err != nil {
		return err
	}
	// Remove the socket if exist to avoid EADDRINUSE
	if err := os.RemoveAll(address); err != nil {
		return err
	}

	serverOpts := []grpc.ServerOption{
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
			streamNamespaceInterceptor,
		)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			unaryNamespaceInterceptor,
		)),
	}

	rpc := grpc.NewServer(serverOpts...)

	// Instantiate the EROFS differ
	d := &diffService{address: containerdAddress}
	service := diffservice.FromApplierAndComparer(d, d)
	diffapi.RegisterDiffServer(rpc, service)

	var opts []snapshot.Opt
	// Instantiate the EROFS snapshotter
	sn, err := snapshot.NewSnapshotter(root, opts...)
	if err != nil {
		return err
	}

	// Convert the snapshotter to a gRPC service,
	// example in github.com/containerd/containerd/contrib/snapshotservice
	ss := snapshotservice.FromSnapshotter(sn)

	// Register the service with the gRPC server
	snapshotsapi.RegisterSnapshotsServer(rpc, ss)

	// Listen and serve
	l, err := net.Listen("unix", address)
	if err != nil {
		return err
	}
	return rpc.Serve(l)
}

func unaryNamespaceInterceptor(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	if ns, ok := namespaces.Namespace(ctx); ok {
		// The above call checks the *incoming* metadata, this makes sure the outgoing metadata is also set
		ctx = namespaces.WithNamespace(ctx, ns)
	}
	return handler(ctx, req)
}

func streamNamespaceInterceptor(srv interface{}, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := ss.Context()
	if ns, ok := namespaces.Namespace(ctx); ok {
		// The above call checks the *incoming* metadata, this makes sure the outgoing metadata is also set
		ctx = namespaces.WithNamespace(ctx, ns)
		ss = &wrappedSSWithContext{ctx: ctx, ServerStream: ss}
	}

	return handler(srv, ss)
}

type wrappedSSWithContext struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedSSWithContext) Context() context.Context {
	return w.ctx
}

type differ interface {
	diff.Applier
	diff.Comparer
}

type diffService struct {
	address string

	differ differ
	loaded uint32
	loadM  sync.Mutex

	diffapi.UnimplementedDiffServer
}

func (a *diffService) getDiffer() (differ, error) {
	if atomic.LoadUint32(&a.loaded) == 1 {
		return a.differ, nil
	}
	a.loadM.Lock()
	defer a.loadM.Unlock()
	if a.loaded == 1 {
		return a.differ, nil
	}

	client, err := containerd.New(a.address)
	if err != nil {
		return nil, nil
	}

	defer atomic.StoreUint32(&a.loaded, 1)
	a.differ = erofsdiff.NewErofsDiffer(client.ContentStore(), []string{})
	return a.differ, nil
}

func (s *diffService) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount, opts ...diff.ApplyOpt) (d ocispec.Descriptor, err error) {
	differ, err := s.getDiffer()
	if err != nil {
		return d, err
	}
	return differ.Apply(ctx, desc, mounts, opts...)
}

func (s *diffService) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (d ocispec.Descriptor, err error) {
	differ, err := s.getDiffer()
	if err != nil {
		return d, err
	}
	return differ.Compare(ctx, lower, upper, opts...)
}
