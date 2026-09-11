package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

const legacyIPTablesNamesPath = "/proc/net/ip_tables_names"

type legacyIPTablesTables interface {
	Names() (map[string]struct{}, error)
}

type procLegacyIPTablesTables struct{ path string }

func (tables procLegacyIPTablesTables) Names() (map[string]struct{}, error) {
	path := tables.path
	if path == "" {
		path = legacyIPTablesNamesPath
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read legacy iptables table names: %w", err)
	}
	names := make(map[string]struct{})
	for _, name := range strings.Fields(string(contents)) {
		names[name] = struct{}{}
	}
	return names, nil
}

type legacyForwardFacts struct {
	exists bool
	active bool
}

type legacyPostroutingFacts struct {
	exists bool
	active bool
}

type legacyIPTablesReader interface {
	Forward(context.Context) (legacyForwardFacts, error)
	Postrouting(context.Context) (legacyPostroutingFacts, error)
}

type fixedLegacyIPTablesReader struct {
	runner directArgvRunner
	tables legacyIPTablesTables
}

func (reader fixedLegacyIPTablesReader) Forward(ctx context.Context) (legacyForwardFacts, error) {
	filter, exists, err := reader.readLoadedTable(ctx, "filter")
	if err != nil || !exists {
		return legacyForwardFacts{}, err
	}
	active, err := iptablesForwardActive(filter)
	if err != nil {
		return legacyForwardFacts{}, fmt.Errorf("inspect existing legacy FORWARD surface: %w", err)
	}
	return legacyForwardFacts{exists: true, active: active}, nil
}

func (reader fixedLegacyIPTablesReader) Postrouting(ctx context.Context) (legacyPostroutingFacts, error) {
	nat, exists, err := reader.readLoadedTable(ctx, "nat")
	if err != nil || !exists {
		return legacyPostroutingFacts{}, err
	}
	active, err := iptablesPostroutingActive(nat)
	if err != nil {
		return legacyPostroutingFacts{}, fmt.Errorf("inspect existing legacy POSTROUTING surface: %w", err)
	}
	return legacyPostroutingFacts{exists: true, active: active}, nil
}

func (reader fixedLegacyIPTablesReader) readLoadedTable(ctx context.Context,
	table string,
) ([]byte, bool, error) {
	if reader.runner == nil || reader.tables == nil {
		return nil, false, errors.New("legacy iptables reader dependencies are incomplete")
	}
	names, err := reader.tables.Names()
	if err != nil {
		return nil, false, err
	}
	if _, exists := names[table]; !exists {
		return nil, false, nil
	}
	plane, err := newIPTablesRulePlane(iptablesFrontendLegacy, reader.runner)
	if err != nil {
		return nil, false, err
	}
	if err := plane.verifyIdentity(ctx); err != nil {
		return nil, false, fmt.Errorf("inspect existing legacy %s table: %w", table, err)
	}
	contents, err := plane.readTable(ctx, table)
	if err != nil {
		return nil, false, fmt.Errorf("inspect existing legacy %s table: %w", table, err)
	}
	return contents, true, nil
}

func emptyLegacyFilterInventory() []byte {
	return []byte("*filter\n:FORWARD ACCEPT [0:0]\nCOMMIT\n")
}

func emptyLegacyNATInventory() []byte {
	return []byte("*nat\n:POSTROUTING ACCEPT [0:0]\nCOMMIT\n")
}
