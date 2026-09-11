# Transfer Lanes

`transferlanes` runs a directory transfer over one or more preconfigured Internet
egress paths on a multi-WAN or multi-homed Linux host. When several paths are
selected, it uses them in parallel. Each path is selected by a local IPv4
address.

Transfer Lanes assigns whole files from the source directory to the selected paths
according to relative weights and exposes each assignment as a read-only view
of the source tree. It then runs rsync, an S3 client, or another transfer
command for each view, replacing one standalone `{}` argument with the view
path.

Transfer Lanes does not configure provider routes, combine multiple paths into one
connection, split individual files, or provide dynamic failover. `transferlanes run`
requires effective root and is intended for trusted commands. Transfer Lanes uses
namespaces to isolate temporary routing and contain child processes, but they
are not a security sandbox. Transfer commands run as root and inherit the
caller's environment.

## Requirements

`transferlanes run` requires:

- a Linux host and effective root;
- `/dev/fuse`;
- network, mount, and PID namespaces;
- veth, policy routing, conntrack/NAT, and either nftables or a compatible
  iptables frontend;
- one or more local IPv4 addresses with usable outbound routes already
  configured on the host.

IPv4 forwarding must already be permitted on each selected provider interface.
Privileged network controllers must leave Transfer Lanes' temporary veth interfaces
unchanged while a run is active.

## Build

Building Transfer Lanes requires Go 1.25 or newer. From the repository root, build a
static Linux binary for `amd64` or `arm64`:

```bash
./scripts/build-development.sh --arch amd64 --output transferlanes
sudo install -m 0755 transferlanes /usr/local/bin/transferlanes
```

## Quick start

First inspect the local IPv4 paths available to Transfer Lanes:

```bash
transferlanes list
```

This inspection is passive: it reads local interface and routing facts without
contacting the Internet or changing the host.

Select the paths to use from that output. The example below gives the first path
twice the share of the second:

```bash
sudo transferlanes run \
  --source /data/batch \
  --network 192.0.2.10@2 \
  --network 198.51.100.20@1 \
  -- rsync --archive {} user@backup.example:/incoming/
```

Files remain whole. Each transfer view preserves the source directory's
basename and relative hierarchy.

## Networks and weights

External diagnostics run only when explicitly requested:

```bash
transferlanes list --network 192.0.2.10 --probe
transferlanes list --measure --measure-duration 5s
```

`--probe` reports the public IP, ASN, and organization observed through each
usable path. `--measure` performs concurrent upload measurements and reports
Mbps for successful paths. Relative weights are shown only when at least two
paths succeed; failed paths receive no weight. Results describe the route to
the selected diagnostic endpoint, not guaranteed performance to every
destination.

For `run`, a network without an explicit weight has weight `1`. Weights may be
positive integers or decimals and are interpreted as a ratio across the
selected group. A run may use a single network.

`--auto-weight` requires at least two networks and measures them before source
allocation. If any selected network cannot be measured, no transfer command is
started. See `transferlanes list --help` and `transferlanes run --help` for the current
diagnostic endpoints and override options.

## Transfer behavior

The transfer command must contain exactly one argument equal to `{}`. Use `--`
to separate Transfer Lanes options from the transfer command. Transfer Lanes replaces the
placeholder and executes the resulting argument vector directly, without
invoking a shell or rewriting tool-specific arguments.

For example, automatic weights can be used with an AWS CLI S3 transfer:

```bash
sudo transferlanes run \
  --source /data/batch \
  --network 192.0.2.10 \
  --network 198.51.100.20 \
  --auto-weight \
  -- aws s3 cp --recursive {} s3://example-bucket/incoming/
```

Transfer Lanes determines source membership before starting transfer commands. Regular
files, including dotfiles, and empty source directories are included. Source
symlinks and special files are omitted by default.

`--follow-symlinks` expands source symlinks at their original names, including
targets outside the source root. An invalid or unreadable target stops the run
before transfer commands start. The resulting read-only views expose ordinary
files and directories rather than symlinks, so transfer tools do not need an
option for following Transfer Lanes-created links.

Files are assigned deterministically from their captured sizes and the selected
weights. A large file can make the observed distribution differ from the
requested ratio because files are never split.

The transfer tool remains responsible for credentials, destination conflicts,
retries, resumable transfers, remote behavior, and its own concurrency. A
failed transfer does not cancel its siblings, but it makes the overall run fail.

## Output

When stdin and stdout are terminals, `run` uses an interactive TUI. Otherwise,
it uses plain output. `--no-tui` forces plain output in a terminal. Display mode
does not change transfer or cleanup behavior.

The TUI gives each visible transfer a fixed pane containing its live terminal
output, and its frame shows the available controls. Focused mode behaves like
the child terminal while Transfer Lanes continues to handle navigation and whole-run
cancellation. After restoring the outer terminal, Transfer Lanes prints one final
screen snapshot per transfer when it has a complete final state.

An interactive run depends on its controlling terminal. Use a terminal
multiplexer such as tmux when the session must survive an SSH disconnect.

Plain output prefixes complete lines with a stable transfer identifier and
coalesces carriage-return progress. Supply `--log-dir` to preserve raw output
for each transfer.

## Cleanup and safety

Transfer Lanes removes the temporary network resources, child processes, and read-only
views created for a run after normal completion, failure, cancellation, or loss
of the controlling process.

Before starting another run, Transfer Lanes attempts to recover stale resources that it
can prove belong to an earlier run. Active runs are left untouched. If ownership
cannot be established safely, Transfer Lanes leaves the resources in place and reports
an error. `transferlanes list` does not perform recovery.

Transfer Lanes does not change provider routes, the host resolver, existing sysctls, or
unrelated firewall rules. It does not flush route tables or remove foreign
network objects.

## Exit status

| Code | Meaning |
| ---: | --- |
| `0` | The command completed successfully. |
| `1` | A runtime operation failed. For `run`, this includes transfer, display, supervision, or cleanup failures, and cancellation by `SIGTERM` or `SIGHUP`. |
| `2` | The command line is invalid. |
| `130` | `run` was interrupted by `SIGINT`. |

Use `transferlanes --help` and `transferlanes <command> --help` for complete command syntax,
options, constraints, and current defaults.

## License

Transfer Lanes is available under the [MIT License](LICENSE).
