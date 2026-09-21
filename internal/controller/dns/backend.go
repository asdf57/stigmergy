package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	routeros "github.com/go-routeros/routeros/v3"
)

const routerOSAPIPort = "8728"

var errUnsupportedRecordType = errors.New("record type is not supported by the RouterOS backing store")

type Connection struct {
	ManagementAddress string
	Username          string
	Password          string
}

type DesiredRecord struct {
	OwnerUID   string
	Name       string
	Type       apigen.DNSRecordSpecType
	Values     map[string]string
	TTLSeconds int64
}

type Backend interface {
	Ensure(context.Context, Connection, DesiredRecord) error
	Delete(context.Context, Connection, string) error
}

type routerOSBackend struct{}

func (routerOSBackend) Ensure(ctx context.Context, connection Connection, desired DesiredRecord) error {
	address, err := netip.ParseAddr(connection.ManagementAddress)
	if err != nil {
		return fmt.Errorf("parse Router management address %q: %w", connection.ManagementAddress, err)
	}
	client, err := routeros.DialContext(ctx, net.JoinHostPort(address.String(), routerOSAPIPort), connection.Username, connection.Password)
	if err != nil {
		return fmt.Errorf("connect and authenticate to RouterOS API: %w", err)
	}
	defer client.Close()

	comment := ownerComment(desired.OwnerUID)
	reply, err := client.RunContext(ctx,
		"/ip/dns/static/print",
		"?comment="+comment,
		"=.proplist=.id,type",
	)
	if err != nil {
		return fmt.Errorf("find owned RouterOS DNS record: %w", err)
	}
	if len(reply.Re) > 1 {
		return fmt.Errorf("found %d RouterOS DNS records owned by DNSRecord UID %q", len(reply.Re), desired.OwnerUID)
	}

	if len(reply.Re) == 1 {
		id := reply.Re[0].Map[".id"]
		if id == "" {
			return errors.New("owned RouterOS DNS record has no identifier")
		}
		if reply.Re[0].Map["type"] != string(desired.Type) {
			if _, err := client.RunContext(ctx, "/ip/dns/static/remove", "=.id="+id); err != nil {
				return fmt.Errorf("replace RouterOS DNS record with changed type: %w", err)
			}
			return addRouterOSRecord(ctx, client, desired, comment)
		}
		arguments := routerOSRecordArguments(desired, comment)
		arguments = append([]string{"/ip/dns/static/set", "=.id=" + id}, arguments...)
		if _, err := client.RunContext(ctx, arguments...); err != nil {
			return fmt.Errorf("update RouterOS DNS record: %w", err)
		}
		return nil
	}

	return addRouterOSRecord(ctx, client, desired, comment)
}

func (routerOSBackend) Delete(ctx context.Context, connection Connection, ownerUID string) error {
	address, err := netip.ParseAddr(connection.ManagementAddress)
	if err != nil {
		return fmt.Errorf("parse Router management address %q: %w", connection.ManagementAddress, err)
	}
	client, err := routeros.DialContext(ctx, net.JoinHostPort(address.String(), routerOSAPIPort), connection.Username, connection.Password)
	if err != nil {
		return fmt.Errorf("connect and authenticate to RouterOS API: %w", err)
	}
	defer client.Close()

	reply, err := client.RunContext(ctx,
		"/ip/dns/static/print",
		"?comment="+ownerComment(ownerUID),
		"=.proplist=.id",
	)
	if err != nil {
		return fmt.Errorf("find owned RouterOS DNS record: %w", err)
	}
	for _, sentence := range reply.Re {
		id := sentence.Map[".id"]
		if id == "" {
			return errors.New("owned RouterOS DNS record has no identifier")
		}
		if _, err := client.RunContext(ctx, "/ip/dns/static/remove", "=.id="+id); err != nil {
			return fmt.Errorf("delete owned RouterOS DNS record %q: %w", id, err)
		}
	}
	return nil
}

func addRouterOSRecord(ctx context.Context, client *routeros.Client, desired DesiredRecord, comment string) error {
	arguments := append([]string{"/ip/dns/static/add"}, routerOSRecordArguments(desired, comment)...)
	if _, err := client.RunContext(ctx, arguments...); err != nil {
		return fmt.Errorf("create RouterOS DNS record: %w", err)
	}
	return nil
}

func routerOSRecordArguments(desired DesiredRecord, comment string) []string {
	arguments := []string{
		"=name=" + desired.Name,
		"=type=" + string(desired.Type),
		"=ttl=" + strconv.FormatInt(desired.TTLSeconds, 10) + "s",
		"=comment=" + comment,
	}
	keys := make([]string, 0, len(desired.Values))
	for key := range desired.Values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		arguments = append(arguments, "="+key+"="+desired.Values[key])
	}
	return arguments
}

func desiredRecord(record registry.DNSRecord) (DesiredRecord, error) {
	values, err := routerOSRecordValues(record.Spec.Type, record.Spec.Value)
	if err != nil {
		return DesiredRecord{}, err
	}
	return DesiredRecord{
		OwnerUID:   record.Metadata.UID,
		Name:       fqdn(record.Spec.Name, record.Spec.Zone),
		Type:       record.Spec.Type,
		Values:     values,
		TTLSeconds: record.Spec.Ttl,
	}, nil
}

func routerOSRecordValues(recordType apigen.DNSRecordSpecType, value string) (map[string]string, error) {
	switch recordType {
	case apigen.DNSRecordTypeA:
		address, err := netip.ParseAddr(value)
		if err != nil || !address.Is4() {
			return nil, fmt.Errorf("A value %q is not an IPv4 address", value)
		}
		return map[string]string{"address": value}, nil
	case apigen.DNSRecordTypeAAAA:
		address, err := netip.ParseAddr(value)
		if err != nil || !address.Is6() {
			return nil, fmt.Errorf("AAAA value %q is not an IPv6 address", value)
		}
		return map[string]string{"address": value}, nil
	case apigen.DNSRecordTypeCNAME:
		return map[string]string{"cname": value}, nil
	case apigen.DNSRecordTypeMX:
		fields := strings.Fields(value)
		if len(fields) != 2 {
			return nil, errors.New("MX value must contain preference and exchange")
		}
		if _, err := strconv.ParseUint(fields[0], 10, 16); err != nil {
			return nil, fmt.Errorf("parse MX preference: %w", err)
		}
		return map[string]string{"mx-preference": fields[0], "mx-exchange": fields[1]}, nil
	case apigen.DNSRecordTypeNS:
		return map[string]string{"ns": value}, nil
	case apigen.DNSRecordTypeSRV:
		fields := strings.Fields(value)
		if len(fields) != 4 {
			return nil, errors.New("SRV value must contain priority, weight, port, and target")
		}
		for index, label := range []string{"priority", "weight", "port"} {
			if _, err := strconv.ParseUint(fields[index], 10, 16); err != nil {
				return nil, fmt.Errorf("parse SRV %s: %w", label, err)
			}
		}
		return map[string]string{
			"srv-priority": fields[0], "srv-weight": fields[1], "srv-port": fields[2], "srv-target": fields[3],
		}, nil
	case apigen.DNSRecordTypeTXT:
		return map[string]string{"text": value}, nil
	default:
		return nil, fmt.Errorf("%w: %s", errUnsupportedRecordType, recordType)
	}
}

func fqdn(name, zone string) string {
	absolute := strings.HasSuffix(name, ".")
	name = strings.TrimSuffix(name, ".")
	zone = strings.TrimSuffix(zone, ".")
	if name == "@" || name == "" {
		return zone
	}
	if absolute || name == zone || strings.HasSuffix(name, "."+zone) {
		return name
	}
	return name + "." + zone
}

func ownerComment(uid string) string {
	return "stigmergy:dns-record:" + uid
}
