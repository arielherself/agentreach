# Architecture

> A *reach* is a teleoperated remote manipulator: the operator stays behind the
> barrier, the manipulator works inside. That is the whole design. The agent —
> the brain, and the credentials it holds — never leaves your machine. Only the
> observation and action space is remote.

This document is the authoritative description of how AgentReach is built and why. It covers the design decisions, the rejected alternatives, and the constraints that shaped both. For a quick overview, the [README](../README.md) is the right starting point.

## Problem

Coding agents assume their tools act on the machine they run on. Three
constraints break that assumption simultaneously:

1. **The agent cannot be installed on the target.** Many servers cannot run a
   ~300 MB Node SEA or a Bun runtime.
2. **Credentials must not reach the target.** A client's server is untrusted;
   an API key or OAuth token placed there is disclosed.
3. **The target must stay unmodified.** No daemon, no binary, no footprint.

The standard answers each fail one of these: running the agent remotely fails
(1) and (2); an MCP tool server changes the tool names the model sees, which
breaks transparency; a FUSE mount fails loudly on macOS (kernel extension,
reboot) and fails *silently* on an unstable link — stalled I/O in uninterruptible
sleep, with the agent frozen mid-call and no error to reason about.

## Principle

> Every failure must be a value the agent can reason about, never a process
> that stops responding.

An RPC timeout becomes a tool error the model sees, retries, or routes around.
That single property is why reach is built on request/response over SSH rather
than on a filesystem mount.

## Shape

```
┌─ your machine ─────────────────────────────────────────────┐
│  harness (claude / codex / kimi / grok / opencode)         │
│      │ native tool calls — the model sees no new tools     │
│  ┌───▼─────────────────────────────────────┐               │
│  │ adapter (per harness, thin, no fork)    │               │
│  └───┬─────────────────────────────────────┘               │
│      │ argv, hook JSON on stdin, or a generated tool       │
│  ┌───▼─────────────────────────────────────┐               │
│  │ session   target · cwd · capabilities   │  a file,      │
│  │           tier decision                 │  not a daemon │
│  ├─────────────────────────────────────────┤               │
│  │ fileops   posix · pipe · helper          │               │
│  ├─────────────────────────────────────────┤               │
│  │ transport ssh · docker · podman · local │               │
│  └───┬─────────────────────────────────────┘               │
└──────┼─────────────────────────────────────────────────────┘
       │  system ssh (multiplexed where the client supports it)
┌──────▼─────────────────────────────────────────────────────┐
│ target: stock sshd only. no node, no python, no reach bits │
└────────────────────────────────────────────────────────────┘
```

Only the top layer is harness-specific.

## There is no daemon

Session state — which target, which directory, which tier, what the target's
userland supports — lives in a file under `~/.reach`. Every reach invocation is
a short-lived process that reads it.

A daemon would buy connection reuse. SSH's `ControlMaster` already provides
that — measured against real hosts at 4–5× faster per command than
reconnecting: 171 ms against 772 ms on one, 557 ms against 2.85 s on another.
Paying for it a second time would mean a lifecycle, a socket, crash recovery,
version skew between a running daemon and an upgraded binary, and orphaned
processes holding connections to someone else's server.

It would buy one thing more, and this document used to say it would not.
`ControlMaster` removes the cost of opening a *connection*; it does not remove
the cost of opening a *channel*, and reach opens one channel per command.
Measured against a host 200 ms away: **0.51 s** for a command on a new channel
of an established master, **0.20 s** for one on a channel already running a
shell. A resident process holding that channel open would save the difference
on every tool call, and nothing else reach can do will.

### Why that difference is still not worth a daemon

Because of what it is a difference *of*. Measured across five real agent
sessions against that same host — 830 commands, 7.1 hours of wall clock:

| | |
|---|---|
| median gap between one command and the next | 16.8 s |
| gaps shorter than 1 s | 2% |
| time spent inside commands at all | 21.3% of wall clock |
| **saving 0.31 s per command** | **1.0% of wall clock** |
| removing transport cost entirely | 1.7% of wall clock |

The fourth row is the change. The fifth is the ceiling on every version of it,
including a perfect one with no daemon and no risk. An agent spends about
seventeen seconds thinking between tool calls, so a third of a second in front
of each one is already paid for by something else.

Three shapes were costed before the measurement settled it, and are recorded so
the next person does not re-derive them: teaching the long-lived exec-server to
multiplex commands over the handler channel it already holds (helps Codex only,
and needs a real streaming-and-signalling protocol where today there is one
request in flight); a resident holder with a socket under `~/.reach` (helps
every seam, and is the daemon this section argues against); and having `reach
<target> claude` stay as the harness's parent instead of `execve`-ing into it,
passing a socket down through the environment (free lifecycle, exact teardown,
but it makes the fallback path in `launch.go` the normal path for a program
that owns the terminal — see the comment there on signals and job control).

The rule underneath is worth stating once rather than rediscovering per
decision: **optimise the latency someone waits on, not the latency a model turn
already hides.** Binding a session used to be thirty seconds of an operator
watching `probing build-box ...` with nothing else happening, which is why it
was worth collapsing from eight round trips to one. A tool call is half a
second inside a seventeen-second gap.

Two things would change the answer. An agent an order of magnitude faster
between turns would move the ratio outright; so would `reach exec` becoming
something people drive from shell scripts, with no model in the loop, where
that 0.31 s is most of every call.

One consequence is worth stating early, because it shapes the tier design more
than anything else: **reach runs one process per tool call.**

Sharing one connection has a limit that a daemon would have had too: sshd caps
concurrent channels per connection with `MaxSessions`, 10 by default, and reach
runs one channel per tool call. An agent that fans out past that has its
eleventh tool call refused — `administratively prohibited`, ssh exit 255, which
reach used to report as "command did not complete". reach now opens a second
connection instead, up to a small bound, and says so on stderr. The retry is
safe in a way retrying a failed command generally is not: a refused channel
means the remote shell was never started, so there is nothing to have
half-happened. A connection that dropped mid-command is never retried, because
it says nothing about whether the command ran.

## A target names its session

`reach <target> <command>` — `reach build-box claude` — binds a session and
runs the command against it in one line. The two-step form (`reach up`, then
`reach claude`) still exists for sessions that outlive a single agent, but it is
no longer the way in, for two reasons.

The first is arithmetic. A session that exists for the length of one agent
should not cost three commands to create, use and destroy.

The second is a trap. `reach up` defaults the session name to `default`, so a
second `reach up` with no `--name` silently replaced the first session — and
because the harness launchers resolve their target through that same name, an
agent already working through it started running its commands on a different
machine. reach could always hold many sessions at once; they had to be named by
hand, and nothing said so. In the one-shot form the target names the session
(`build-box`, `build-box-app`), a name already taken by a *different* target
gets a numbered variant rather than being overwritten, and `reach up` now says
so out loud when it repoints a name.

A second command against a target reuses that target's session instead of
probing again — a probe costs an authentication and a round trip, and asking a
host questions it has already answered would be work with no answer attached.
What is not skipped is the connection: reuse re-authenticates while the operator
is still present, because every connection after that runs in batch mode and
cannot prompt for a passphrase or a hardware token.

A bare word is read as a host only when the operator's own ssh configuration or
hosts file names it, or when it is an address or a dotted name. DNS is
deliberately not consulted: resolvers that answer for every name are common
enough that a lookup would turn `reach stauts` into a connection attempt on
exactly the networks where that mistake is hardest to see.

## Platforms

reach runs on Linux, macOS and Windows, and targets any POSIX host. The split
matters because the two sides need different things: the operator's machine runs
a harness and needs to intercept its shell, while the target only ever sees
shell commands.

Every operating-system difference lives in two files — `platform_other.go` and
`platform_windows.go` — so the cost of supporting Windows is visible in one
place rather than spread through the adapters. Windows needs four things Unix
gives for free: a launcher that is not `execve`, shims that are not symlinks,
executability decided by `PATHEXT` rather than a mode bit, and a search-path
variable matched case-insensitively.

The fifth difference cannot be abstracted away. Win32-OpenSSH does not implement
`ControlMaster`, so a Windows operator pays a full connection setup per command
rather than ~7 ms on a shared one. That is not a portability detail: the
argument for [having no daemon](#there-is-no-daemon) rests on ControlMaster
already providing connection reuse, and on Windows that premise is false. The
gap it leaves is also an order of magnitude larger than the channel-reuse gap
that section weighs and dismisses — seconds per command against a third of a
second — so a Windows operator on a distant host is the one case where the
arithmetic there could come out the other way. reach therefore *probes* for multiplexing rather than assuming it, records
the answer, and reports it — see [WINDOWS.md](WINDOWS.md).

That probe runs before any other question a session asks, because its answer
decides what asking costs. Everything else the target is asked — what its
userland provides, what `PATH` a login shell would give, whether the link is
8-bit clean in each direction, where a login lands, whether the workspace is
there — travels in one shell program on the connection the probe established.
Binding a session is therefore one authentication and one round trip, whatever
the latency: measured against a host 200 ms away, 2.6 s, against 12.4 s when
each question opened a connection of its own.

## reach uses the system ssh, not a Go SSH library

Users reach real hosts through jump hosts, certificate authorities, hardware
tokens, `gpg-agent`, Kerberos, 1Password, and `Match exec` blocks.
Reimplementing that surface faithfully is not realistic, and getting it subtly
wrong strands people on exactly the hosts they most need to reach. reach shells
out to the `ssh` they already have, so `~/.ssh/config` keeps working unchanged.

The cost is that ssh reports its own failures as exit 255, which is
indistinguishable from a command that genuinely exited 255. reach therefore
carries the real status in-band behind an unguessable marker, and its
**absence** is the signal that the transport, rather than the command, failed.
Getting this wrong in either direction is bad: a transport failure reported as a
command failure sends the agent chasing a phantom bug, and a command failure
reported as a transport failure makes reach retry something that must not be
retried.

## A command reach stops waiting for is signalled, not just abandoned

Closing the channel does not end a command. A stock sshd offers no way to signal
a remote process group, and a command that produces no output never notices that
the pipe it would have got `EPIPE` from has gone — `sleep 600` survives a closed
channel, and so does a quiet build.

So a streamed command reports its own process group before it starts. sshd calls
`setsid` for every session it opens, which makes the shell it starts the leader
of a group holding everything the command spawns; that shell prints `$$` on
stderr behind the run's random marker, and reach strips the line. When reach
stops waiting — a timeout, or the harness's SIGTERM — it opens a second channel
on the same connection and sends `kill -TERM -- -PGID`, then SIGKILL two seconds
later from a detached subshell.

This cannot hit the wrong group. A group id is the pid of the process that
created it, and that process is the shell reach is talking to, so `-PGID` names
either the command's group or no group at all; where the shell does not lead one
(a container runtime that does not `setsid`), the group signal fails and the
shell alone is signalled. A process that starts its own session — a daemon,
`setsid`, `nohup` into the background — is outside the group and survives, and
the error says so and how to check. A local target is reach's own child and is
killed directly.

Under Claude Code the shell prefix applies no timeout of its own. Claude Code
bounds the Bash tool itself (observed on 2.1.295: SIGTERM to the prefix, or the
call moved to the background with a ten-minute limit there), so the session's
`--timeout` would only be a second, shorter limit the model cannot see or raise.

## The two interfaces

reach separates *reaching a target* from *performing file operations on it*.

```go
// internal/transport — how to reach a target and run a command.
type Transport interface {
    Run(ctx, ExecRequest) (ExecResult, error)   // to completion, bounded output
    Open(ctx, command string) (Stream, error)   // long-lived, piped stdio
    Describe() string
    Close() error
}

```

```go
// internal/fileops — how to act on files, in four interchangeable ways.
type FileOps interface {
    Read(ctx, path string, off, n int64) ([]byte, error)
    Write(ctx, path string, data []byte, mode fs.FileMode) error
    Stat(ctx, path string) (*FileInfo, error)
    List(ctx, path string) ([]FileInfo, error)
    Mkdir(ctx, path string, mode fs.FileMode) error
    Remove(ctx, path string, recursive bool) error
    Rename(ctx, from, to string) error
    Search(ctx, SearchRequest) ([]Match, error)
    Glob(ctx, root, pattern string) ([]string, error)
    Hash(ctx, path string) (string, error)
    Tier() Tier
    Close() error
}
```

`Search` and `Glob` are first-class operations, not helpers derived from `List`,
and no tier has ever implemented them any other way: a search is one command on
the target, and only matches cross the network.
They execute **on the target** and return only matches, at every tier — which is
precisely what a mount cannot do, and the main reason reach beats one on the
operation that matters most. Deriving them client-side would mean dragging every
candidate file across the network to answer a question the target could have
answered locally.

## Tiers

Three strategies implement `FileOps`. They share almost no code — a shell
pipeline, a Python handler, a Go binary — and a user cannot tell which is in
use. That interchangeability claim is only worth something
because every tier runs one identical conformance suite
(`internal/fileops/fileopstest`): over the local transport in unit tests, and
over a real sshd in `test/integration`, which additionally asserts that a file
written through any tier reads back byte-for-byte through every other. A tier
that cannot pass it does not ship.

Full detail, including what each tier requires and writes, is in
[TRANSPORTS.md](TRANSPORTS.md). The architectural points:

- **Every tier answers one file operation in one round trip.** A protocol that
  cannot — SFTP, which hands out a handle before it will read — was implemented
  and then removed for that reason, and the reasoning is kept in
  [TRANSPORTS.md](TRANSPORTS.md#why-there-is-no-sftp-tier).
- **Tier 0 is the floor and needs only a POSIX shell.** Everything above it is
  an optimisation that is never required.
- **Negotiation follows measurement, not the tier numbering.** The numbers rank
  capability; reach ranks tiers by what they actually cost in a
  process-per-call design, where an interpreter or binary starting up is pure
  overhead. That makes tier 1 the negotiated choice where available, and the
  nominally fastest the helper tier the slowest to start.
- **A pinned tier is an instruction.** `--fileops=X` fails rather than
  substituting something else, because a `reach status` reporting a tier the
  session is not using is a lie the operator will act on. An autonegotiated tier
  may still step down, and says so on stderr.
- **Only the helper tier writes to the target**, and only when the operator
  names it: autonegotiation stops below it. Everything it installs is listed by
  `reach doctor` and removed by `reach helper uninstall`.

## Two modes

A session runs in one of two modes, chosen by what the harness's file tools can
be made to do.

### `exec` — command execution is remoted; file tools are not

Correct and zero-copy. Used when the harness's file tools cannot be redirected
(Claude Code, Codex, Kimi) and mirroring is not wanted.

Because the harness's native `Read`/`Edit`/`Write` would still silently act on
the **local** filesystem — reading the wrong file while the agent believes it is
remote — reach **denies those tools** in the generated harness config for this
mode. Silent wrong-target file access is the worst failure this design can
produce, so it is made structurally impossible rather than documented as a
caveat. The agent uses the shell for file access, which is transparently remote.

For harnesses that *can* shadow tools by name (opencode), `exec` mode is full
fidelity: `read`/`write`/`edit`/`grep`/`glob` are backed by the target directly,
and no mirroring is needed.

### `mirror` — the file a tool is about to touch, materialised

Gives Claude Code native file tools without MCP and without FUSE. A `PreToolUse`
hook rewrites the tool's `file_path` to a local copy that reach fetches at that
moment; a `PostToolUse` hook writes the result back.

This is deliberately **not** a sync engine, and not a bulk copy of the
workspace. Nothing is mirrored until a tool asks for it, and there is no
background reconciliation. A sync engine has to answer questions reach has no
good answer to — both sides changed, deleted or never fetched — and getting them
wrong loses the operator's work. Fetching exactly the file a tool is about to
touch, at the moment it touches it, raises none of them.

**Writes are guarded by a digest taken at fetch time.** If the file changed on
the target in between — a build, a deploy, another session — the write is
refused with an error the agent can act on, rather than overwriting from a stale
base. A refusal the agent can see is always better than a quiet loss.

**`Grep` and `Glob` stay denied in mirror mode.** The mirror holds only files
already opened, so a search across it would report confidently incomplete
results, and an agent told "no matches" concludes the code does not exist. The
agent is pointed at `rg`/`find` over the shell, which run on the target and are
faster anyway.

**Where the mirror lives.** Under `~/.reach/mirror/<session>/`, with the
target's absolute path reproduced beneath it: `/srv/app/main.go` becomes
`~/.reach/mirror/default/srv/app/main.go`. Placing it at the identical absolute
path would make compiler output and stack traces line up with no translation at
all, which is genuinely attractive — but it would require reach to write to
`/srv` on the operator's own machine. That is usually impossible without root,
and an unacceptable thing for this tool to do even where it is possible.

The residual cost is that the agent sees a local path in a `Read` result and
could try to use it in a shell command, where it does not exist. The mirror-mode
system prompt tells it to use the target's own paths, and the hook leaves every
path outside the workspace alone so the harness's own files keep working.

Paths are cleaned before being joined to the mirror root, so a path containing
`..` cannot escape it. File paths can originate in content read from an
untrusted target, which makes that a real attack path rather than a theoretical
one.

**An honest assessment: mirror is the weakest of reach's mechanisms, kept
because Claude Code offers nothing stronger.** Its known costs, stated plainly
rather than discovered by the operator:

- **Reads can be stale; only writes are guarded.** The digest protects the
  write-back, but a file that changes on the target right after the fetch is
  read and reasoned about in its old state. On a host with active builds or
  deploys, that window is real.
- **It is read-modify-write over the network.** An `Edit` rewrites the whole
  file back, not the changed lines.
- **`Grep`/`Glob` stay denied even here**, so the mode does not actually
  deliver the full native tool surface it appears to promise.
- **The path leak** above is mitigated by a system-prompt instruction, not
  eliminated — and instructions to models are probabilistic.

Where a harness offers a direct seam — opencode's tool shadowing, or Codex's
exec-server protocol, both of which execute file operations *on the target*
with no copy, no staleness window, and full search fidelity — that mechanism
is strictly better and mirror should not be used. Mirror exists for the one
harness whose tools can be neither shadowed nor redirected, only re-pointed at
a local file. Prefer `exec` mode for shell-shaped work, prefer a direct-seam
harness for edit-heavy work, and treat mirror as the fallback for when it must
be Claude Code *and* it must be native file tools.

## Harness adapters

| harness | seam | shell | file tools | verified |
|---|---|---|---|---|
| Claude Code | `CLAUDE_CODE_SHELL_PREFIX` | ✓ remote | exec/mirror | yes, 2.1.233 |
| Codex | exec-server (`environments.toml`) | ✓ remote (via exec-server) | ✓ remote (via exec-server) | yes, 0.148 — see [harnesses/codex.md](harnesses/codex.md) |
| Kimi Code | `KIMI_SHELL_PATH` → shim (npm patch) | ✓ remote | denied (use shell) | yes, 0.37.2 — see [harnesses/kimi.md](harnesses/kimi.md) |
| opencode | generated tools shadowing built-ins by name | ✓ remote | ✓ remote | yes — see [harnesses/opencode.md](harnesses/opencode.md) |
| Goose (Block) | `GOOSE_SHELL` env var | ✓ remote | denied via `available_tools: [shell]` | yes — see [harnesses/goose.md](harnesses/goose.md) |
| Crush (Charm) | server mode (`crush server --host`) | ✓ remote | ✓ remote | yes — see [harnesses/crush.md](harnesses/crush.md) |
| Gemini CLI | PATH shim (bare `bash` name) | ✓ remote | denied via `excludeTools` | yes — see [harnesses/gemini.md](harnesses/gemini.md) |
| Grok Build | `$SHELL` → shim | ✓ remote (envelope unwrapped) | removed via agent-profile `disallowedTools` | yes, 1.0.5 — see [harnesses/grok.md](harnesses/grok.md) |

For harnesses where the seam could regress across versions (Codex, Kimi,
Goose, Gemini), `reach harness verify` drives the harness against an embedded
offline mock model and checks where a scripted command actually ran.  The launch
guard refuses versions measured to bypass the seam.  The `--task-prefix` flag
probes a specific operation type (file read, file write) in addition to the
default shell execution canary.

No harness is forked. Claude Code and Codex keep their own authentication, so
subscription logins continue to work and no key is introduced anywhere.

Harnesses want a *program path* for their shell hook, not a command line —
Claude Code stats the value of `CLAUDE_CODE_SHELL_PREFIX` directly, so
`reach shell-prefix` would be looked up as a single filename and fail. reach
dispatches on `argv[0]` through a symlink instead, which costs nothing per tool
call.

### The Claude Code envelope

Claude Code wraps every Bash call. reach parses that envelope rather than
forwarding it, because two segments are local-only. Full shape and rationale in
[RESEARCH.md](RESEARCH.md); the operational summary:

- **strip** `source <local-snapshot>.sh` — references local paths, and leaks the
  local username and directory layout to the remote host
- **strip** `pwd -P >| /tmp/claude-<rand>-cwd` — this is how `cd` persists
  between calls; forwarded verbatim it would be written on the *remote* while
  Claude Code reads it *locally*, and `cd` would silently stop working. reach
  tracks cwd itself and writes the local file the envelope named.
- **forward** everything else

Envelope shape is version-specific, so `internal/envelope` parses defensively,
falls back to forwarding the whole string when the shape is unrecognised, and
is covered by a conformance test that fails when a Claude Code upgrade changes
it.

## Security posture

reach exists because the target is not trusted. Consequences, in full in
[SECURITY.md](SECURITY.md):

- No credential, token, or key is ever sent to the target.
- SSH agent forwarding is **refused by default**. On a host with a hostile root,
  a forwarded agent socket lets that host authenticate as you everywhere else
  you can reach.
- The harness's shell snapshot is stripped from every forwarded command, because
  sourcing it would disclose your username and directory layout to the target
  for no benefit. reach does **not** inspect or rewrite arbitrary commands: a
  false positive that mangled one would be worse than the leak it prevented.
- Output from the target is **untrusted input**. It flows into the context of an
  agent that holds your credentials and can write to your local disk; reach
  frames it as untrusted data.

## Environment

Everything below is optional; reach works with none of it set.

| Variable | Effect |
|---|---|
| `REACH_HOME` | Where sessions, mirrors and audit logs live. Default `~/.reach`. Setting it per-shell gives you independent sets of sessions. |
| `REACH_SESSION` | The session commands use when `--session` is absent. `reach claude` and the other harness launchers set it for the process they start, which is how a harness's tool calls find the right target, and `reach <target> <command>` sets it to the session the target named. |
| `REACH_SSH_CONFIG` | An alternate `ssh_config`, passed as `ssh -F`. Lets reach's connections be configured separately from your interactive ones without duplicating host definitions. |
| `REACH_CONTROL_PERSIST` | How long the authenticated connection outlives its last command — a duration, or `yes` to keep it until `reach down`. Default one hour. Every connection after `reach up` runs in batch mode and cannot prompt, so on a host wanting a password or a hardware token this is the difference between a reconnect and a failed tool call. |
| `REACH_NO_AUDIT` | Set to any value to stop recording what reach did. A record of every command is occasionally the wrong thing to keep — a shared machine, a command line carrying a secret — and that judgement is the operator's. |
| `REACH_HELPER_BINARY` | A helper binary to install instead of the one reach would locate or build. For the helper tier only. |
| `REACH_LOCAL_SHELL` | Windows only: a POSIX shell to use for `local://` targets. reach will not guess one, because guessing wrong runs your command under a shell that quotes differently. |

The session file records the target, the negotiated tier, and the capability
probe's results. It carries a schema version, and a file from a newer reach is
refused rather than partly read: `encoding/json` drops fields it does not
recognise without a word, and a session reach has only partly understood is
exactly the uncertainty about *which machine* this project exists to remove.
