package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/ZhuzhuNo3/transferlanes/internal/listnetworks"
)

func writeNetworkList(output io.Writer, result listnetworks.Result,
	includeProbe, includeMeasure bool,
) error {
	headings := []string{"LOCAL_IP", "IFACE", "ROUTE_TABLE", "GATEWAY", "LOCAL_STATUS"}
	if includeProbe {
		headings = append(headings, "PUBLIC_IP", "ASN", "ORGANIZATION", "PROBE_STATUS")
	}
	if includeMeasure {
		headings = append(headings, "MBPS", "WEIGHT", "MEASURE_STATUS")
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, strings.Join(headings, "\t")); err != nil {
		return err
	}
	for _, row := range result.Rows() {
		fields := localFields(row)
		if includeProbe {
			fields = append(fields, probeFields(row)...)
		}
		if includeMeasure {
			fields = append(fields, measureFields(row)...)
		}
		if _, err := fmt.Fprintln(table, strings.Join(fields, "\t")); err != nil {
			return err
		}
	}
	return table.Flush()
}

func localFields(row listnetworks.Row) []string {
	gateway := "direct"
	if value, present := row.Gateway(); present {
		gateway = value.String()
	}
	status := "runnable"
	if !row.Runnable() {
		status = row.LocalReason()
	}
	return []string{row.LocalIP().String(), cleanTableValue(row.InterfaceName()),
		strconv.Itoa(row.RouteTable()), gateway, cleanTableValue(status)}
}

func probeFields(row listnetworks.Row) []string {
	status, present := row.Probe()
	if !present || !status.Attempted() {
		return []string{"", "", "", ""}
	}
	if err := status.Err(); err != nil {
		return []string{"", "", "", cleanTableValue(err.Error())}
	}
	publicIP := ""
	if value, ok := status.PublicIP(); ok {
		publicIP = value.String()
	}
	organization := ""
	if value, ok := status.Organization(); ok {
		organization = value
	}
	return []string{publicIP, strconv.FormatUint(uint64(status.ASN()), 10),
		cleanTableValue(organization), "ok"}
}

func measureFields(row listnetworks.Row) []string {
	status, present := row.Measure()
	if !present || !status.Attempted() {
		return []string{"", "", ""}
	}
	if err := status.Err(); err != nil {
		return []string{"", "", cleanTableValue(err.Error())}
	}
	weight := ""
	if value, ok := status.Weight(); ok {
		weight = strconv.FormatUint(value, 10)
	}
	return []string{strconv.FormatFloat(status.Mbps(), 'f', 2, 64), weight, "ok"}
}

func cleanTableValue(value string) string {
	return strings.TrimSpace(strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value))
}
