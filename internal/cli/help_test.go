package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/probe"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
)

func TestHelpPagesMatchPublicContract(t *testing.T) {
	wants := map[HelpPage]string{
		RootHelpPage: rootHelpContract,
		ListHelpPage: fmt.Sprintf(listHelpContract,
			probe.DefaultEndpointURL, throughput.DefaultEndpointURL, defaultMeasureDuration),
		RunHelpPage: fmt.Sprintf(runHelpContract,
			throughput.DefaultEndpointURL, defaultMeasureDuration),
	}
	for page, want := range wants {
		t.Run(page.String(), func(t *testing.T) {
			got := page.Text()
			if got != want {
				t.Fatalf("help text mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
			}
			if !strings.HasSuffix(got, "\n") {
				t.Fatal("help text does not end with a newline")
			}
			if strings.Contains(got, "\x1b[") {
				t.Fatal("help text contains an ANSI control sequence")
			}
			for lineNumber, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
				if len(line) > 80 {
					t.Errorf("line %d is %d bytes: %q", lineNumber+1, len(line), line)
				}
			}
		})
	}
}

const rootHelpContract = `transferlanes runs a directory transfer over one or more preconfigured outbound
network paths.

Usage:
  transferlanes <command> [options]

Commands:
  list  Inspect local outbound network paths.
  run   Transfer a directory across selected outbound network paths.

Options:
  -h, --help  Show help.
  --version   Show version information.

Run 'transferlanes <command> --help' for more information about a command.
`

const listHelpContract = `Inspect local outbound network paths.

Usage:
  transferlanes list [options]

By default, list reads local address and routing facts only. It does not
contact external services or measure throughput.

Selection:
  --network <IPv4>  Select a local IPv4 address; repeatable and order
                    preserving.
  --all             Show all local IPv4 addresses; incompatible with
                    --network.

Diagnostics:
  --probe
      Observe public IP, ASN, and organization for usable selected networks.
  --probe-url <URL>
      Override the probe endpoint; requires --probe.
      Default: %s
  --measure
      Measure upload throughput for usable selected networks.
  --measure-url <URL>
      Override the upload endpoint; requires --measure.
      Default: %s
  --measure-duration <time>
      Override the upload window; requires --measure. Default: %s.

Measurements report Mbps for every successful network. Relative weights are
computed only when at least two networks succeed; failed networks get no
weight.

Options:
  -h, --help  Show this help.

Examples:
  transferlanes list
  transferlanes list --all
  transferlanes list --network 192.0.2.10 --probe
  transferlanes list --measure --measure-duration 5s
`

const runHelpContract = `Transfer a directory across selected outbound network paths.

Usage:
  transferlanes run --source <directory> --network <IPv4>[@weight]...
             [options] [--]
             <command containing one standalone {} argument>

Run requires Linux and effective root.

Source:
  --source <directory>
      Required normalized absolute source other than /. The final path
      component must be a real directory, not a symlink; ancestor components
      may be symlinks.
  --follow-symlinks
      Expand source symlinks under their logical names, including targets
      outside the source root. Broken, cyclic, or unreadable targets fail
      before commands. Source symlinks are omitted by default.

Regular files and empty directories are transferred. Special files are
omitted. Transfer views contain ordinary files and directories, not symlinks.

Networks:
  --network <IPv4>[@weight]
      Select a local IPv4 outbound path; repeatable and required. Weight is a
      positive integer or decimal, defaults to 1, and is relative to the full
      selected group.
  --auto-weight
      Measure at least two unweighted networks before file allocation.
  --measure-url <URL>
      Override the automatic measurement endpoint.
      Default: %s
  --measure-duration <time>
      Override the automatic measurement window. Default: %s.
  --dns <IP>
      Override DNS used by transfer namespaces; repeatable. The default uses
      the host resolver configuration.

Command:
  The command must contain exactly one argument equal to {}. An explicit --
  ends Transfer Lanes options; otherwise the first non-option starts command
  argv.
  Each transfer receives a mutually exclusive read-only directory view in
  place of {}. Transfer Lanes executes argv directly without a shell or
  tool-specific rewriting.

Display and logs:
  --no-tui
      Use plain output even when stdin and stdout are terminals.
  --mouse
      Start the interactive TUI in target mouse mode; inert in plain mode.
  --log-dir <directory>
      Preserve byte-exact raw logs for each transfer.

Options:
  -h, --help  Show this help.

Examples:
  transferlanes run --source /data/source --network 192.0.2.10 -- \
    rsync -a {} user@backup.example:/incoming/
  transferlanes run --source /data/source \
    --network 192.0.2.10 --network 198.51.100.20 -- \
    aws s3 cp --recursive {} s3://example-bucket/path/
`
