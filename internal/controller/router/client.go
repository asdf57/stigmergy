package router

import (
	"context"
	"fmt"

	routeros "github.com/go-routeros/routeros/v3"
)

// APIProber verifies that a RouterOS API endpoint accepts authenticated
// commands. Implementations must honor cancellation through ctx.
type APIProber interface {
	Probe(ctx context.Context, address, username, password string) error
}

type routerOSAPIProber struct{}

func (routerOSAPIProber) Probe(ctx context.Context, address, username, password string) error {
	client, err := routeros.DialContext(ctx, address, username, password)
	if err != nil {
		return fmt.Errorf("connect and authenticate: %w", err)
	}
	defer client.Close()

	if _, err := client.RunContext(ctx, "/system/resource/print", "=.proplist=version"); err != nil {
		return fmt.Errorf("query system resources: %w", err)
	}
	return nil
}
