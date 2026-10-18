package router

import (
	"context"
	"fmt"
	"strconv"
	"time"

	routeros "github.com/go-routeros/routeros/v3"
)

// RouterClient exposes only the address operations used by the reconciler.
type RouterClient interface {
	Probe(context.Context) error
	ListAddresses(context.Context) ([]RouterOSAddress, error)
	AddSubnet(context.Context, RouterOSAddress) error
	UpdateSubnet(context.Context, RouterOSAddress) error
	RemoveSubnet(context.Context, string) error
}

// ClientFactory binds one reconciliation to its resolved endpoint and credentials.
type ClientFactory func(address, username, password string) RouterClient

type RouterOSApi struct {
	address  string
	username string
	password string
}

// RouterOSAddress is one /ip/address entry, including its observed RouterOS flags.
type RouterOSAddress struct {
	ID              string
	Address         string
	Network         string
	Interface       string
	ActualInterface string
	Comment         string
	Disabled        bool
	Invalid         bool
	Dynamic         bool
	Slave           bool
}

func NewRouterOSApi(address, username, password string) *RouterOSApi {
	return &RouterOSApi{
		address:  address,
		username: username,
		password: password,
	}
}

func (r RouterOSApi) Probe(ctx context.Context) error {
	if _, err := r.run(ctx, "/system/resource/print", "=.proplist=version"); err != nil {
		return fmt.Errorf("query system resources: %w", err)
	}
	return nil
}

// AddSubnet creates an enabled static address with an explicit ownership comment.
func (r RouterOSApi) AddSubnet(ctx context.Context, address RouterOSAddress) error {
	_, err := r.run(ctx, append([]string{"/ip/address/add"}, addressArguments(address)...)...)
	if err != nil {
		return fmt.Errorf("add subnet: %w", err)
	}
	return nil
}

// UpdateSubnet changes an owned address in place rather than removing it first.
func (r RouterOSApi) UpdateSubnet(ctx context.Context, address RouterOSAddress) error {
	if address.ID == "" {
		return fmt.Errorf("update subnet requires a RouterOS identifier")
	}
	_, err := r.run(ctx, append([]string{"/ip/address/set", "=.id=" + address.ID}, addressArguments(address)...)...)
	if err != nil {
		return fmt.Errorf("update subnet: %w", err)
	}
	return nil
}

func (r RouterOSApi) RemoveSubnet(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("remove subnet requires a RouterOS identifier")
	}
	_, err := r.run(ctx, "/ip/address/remove", "=.id="+id)
	if err != nil {
		return fmt.Errorf("remove subnet: %w", err)
	}
	return nil
}

func addressArguments(address RouterOSAddress) []string {
	return []string{"=address=" + address.Address, "=network=" + address.Network,
		"=interface=" + address.Interface, "=comment=" + address.Comment, "=disabled=false"}
}

// run bounds each connection/command and honors cancellation of the reconciliation.
func (r RouterOSApi) run(ctx context.Context, arguments ...string) (*routeros.Reply, error) {
	commandContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, err := routeros.DialContext(commandContext, r.address, r.username, r.password)
	if err != nil {
		return nil, fmt.Errorf("connect and authenticate: %w", err)
	}
	defer client.Close()
	return client.RunContext(commandContext, arguments...)
}

// ListAddresses reads and decodes all configured and dynamic IP address entries.
func (r RouterOSApi) ListAddresses(ctx context.Context) ([]RouterOSAddress, error) {
	reply, err := r.run(ctx, "/ip/address/print")
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}
	return parseRouterOSAddresses(reply)
}

// parseRouterOSAddresses translates wire strings without silently accepting invalid flags.
func parseRouterOSAddresses(reply *routeros.Reply) ([]RouterOSAddress, error) {
	addresses := make([]RouterOSAddress, 0, len(reply.Re))
	for _, sentence := range reply.Re {
		fields := sentence.Map
		address := RouterOSAddress{
			ID:              fields[".id"],
			Address:         fields["address"],
			Network:         fields["network"],
			Interface:       fields["interface"],
			ActualInterface: fields["actual-interface"],
			Comment:         fields["comment"],
		}
		// RouterOS may omit flags that are false, depending on the returned entry.
		for _, flag := range []struct {
			name        string
			destination *bool
		}{
			{"disabled", &address.Disabled},
			{"invalid", &address.Invalid},
			{"dynamic", &address.Dynamic},
			{"slave", &address.Slave},
		} {
			value, present := fields[flag.name]
			if !present {
				continue
			}
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return nil, fmt.Errorf("parse address %q flag %s: %w", address.ID, flag.name, err)
			}
			*flag.destination = parsed
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}
