package cli

import (
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/probe"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
)

func (page HelpPage) Text() string {
	switch page {
	case RootHelpPage:
		return rootHelpText
	case ListHelpPage:
		return fmt.Sprintf(listHelpTemplate,
			probe.DefaultEndpointURL, throughput.DefaultEndpointURL, defaultMeasureDuration)
	case RunHelpPage:
		return fmt.Sprintf(runHelpTemplate,
			throughput.DefaultEndpointURL, defaultMeasureDuration)
	default:
		return ""
	}
}

func (page HelpPage) String() string {
	switch page {
	case RootHelpPage:
		return "root"
	case ListHelpPage:
		return "list"
	case RunHelpPage:
		return "run"
	default:
		return "unknown"
	}
}

const rootHelpText = `transferlanes parallelizes the transfer of a directory across multiple outbound
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

const listHelpTemplate = `Inspect local outbound network paths.

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

const runHelpTemplate = `Transfer a directory across selected outbound network paths.

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
