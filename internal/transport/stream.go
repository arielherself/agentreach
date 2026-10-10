package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/agentreach/internal/reach"
)

// RunStream executes a command and streams its output as it arrives.
//
// Buffering a command's output until it exits is fine for a file read and
// wrong for a ten-minute build: the operator watches a dead terminal and
// cannot tell a slow test suite from a hung connection. Streaming keeps the
// feedback that makes a remote shell feel local.
//
// The exit status still travels in-band behind a random marker, so the
// stdout stream is filtered to remove that marker before the caller sees it.
func RunStream(ctx context.Context, t Transport, req reach.ExecRequest, stdout, stderr io.Writer) (int, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	for {
		code, wrote, err := runStreamOnce(ctx, t, req, stdout, stderr)
		if err == nil || !IsChannelOpenFailure(err.Error()) {
			return code, err
		}
		// A refused channel means the remote shell was never started, so the
		// command can be run again on another connection without any risk of
		// running it twice. Output already handed to the caller would break
		// that reasoning, so it ends the retry even though it should be
		// impossible on a channel that never opened.
		if wrote > 0 {
			return code, err
		}
		o, ok := t.(Overflower)
		if !ok || !o.Overflow() {
			return code, err
		}
		// ssh has already printed its own refusal to the caller's stderr, and
		// on its own that reads like the command failed. Say what reach did
		// about it.
		_, _ = fmt.Fprintf(stderr, "reach: %s had no room for another channel on this connection; "+
			"opened another one and ran the command there\n", t.Describe())
	}
}

// runStreamOnce is one attempt, reporting how many bytes of the command's own
// output reached the caller so RunStream can tell a command that never started
// from one that did.
func runStreamOnce(ctx context.Context, t Transport, req reach.ExecRequest, stdout, stderr io.Writer) (int, int64, error) {
	sentinel := newSentinel()
	pidTag := sentinel + "pid:"
	wrapped := announcePID(pidTag) + wrapWithSentinel(BuildCommand(req), sentinel)

	st, err := t.Open(ctx, wrapped)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = st.Close() }()

	// Cancelling ctx kills the local ssh, but that does not end the reads
	// below. A multiplexed ssh hands its pipes to the master connection, which
	// keeps them open for as long as the remote command runs — so without
	// this, a timeout or a signal waited for the very command it was meant to
	// abandon.
	stopWatch := context.AfterFunc(ctx, func() {
		closeReader(st.Stdout)
		closeReader(st.Stderr)
	})
	defer stopWatch()

	counted := &countingWriter{out: stdout}
	// The target's own complaint is kept as well as forwarded. Without it the
	// failure below was "connection closed before the command completed (status
	// 255)" and nothing else — neither an operator nor RunStream itself could
	// tell a refused channel from a dropped connection.
	tap := &stderrTap{out: stderr, remaining: 8 << 10}
	pid := &pidCapture{out: tap, tag: []byte(pidTag)}

	if len(req.Stdin) > 0 {
		go func() {
			_, _ = st.Stdin.Write(req.Stdin)
			_ = st.Stdin.Close()
		}()
	} else {
		_ = st.Stdin.Close()
	}

	filter := &sentinelFilter{out: counted, sentinel: []byte("\n" + sentinel)}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(pid, st.Stderr)
		pid.flush()
	}()

	_, copyErr := io.Copy(filter, st.Stdout)
	wg.Wait()
	filter.flush()

	if ctx.Err() != nil {
		_ = st.Close()
		_, finished := filter.exitCode()
		if p, ok := pid.value(); ok && !finished && mayOutliveDisconnect(t) {
			if err := signalRemoteGroup(t, p); err == nil {
				return 0, counted.written(), fmt.Errorf("%s: %w%s", t.Describe(), ctx.Err(), signalledNote(p))
			}
		}
		return 0, counted.written(), fmt.Errorf("%s: %w%s", t.Describe(), ctx.Err(), abandonedCommandNote(t))
	}

	waitCode, waitErr := st.Wait()
	wrote := counted.written()
	if copyErr != nil {
		return 0, wrote, fmt.Errorf("%s: reading output: %w", t.Describe(), copyErr)
	}
	if code, ok := filter.exitCode(); ok {
		return code, wrote, nil
	}
	if waitErr != nil {
		return 0, wrote, fmt.Errorf("%s: %w", t.Describe(), waitErr)
	}
	// The marker never arrived, so the command did not run to completion. This
	// is a transport failure and must not be reported to the agent as a plain
	// non-zero exit, which would send it debugging a command that never ran.
	failure := fmt.Sprintf("%s: connection closed before the command completed (status %d)%s",
		t.Describe(), waitCode, complaint(tap.String()))
	return 0, wrote, fmt.Errorf("%s%s", failure, advise(t, failure))
}

// abandonedCommandNote warns that a command reach gave up on may still be
// running where it was started.
//
// It is the fallback for when signalRemoteGroup could not run: the shell never
// announced its pid, or the second channel failed. Then closing the channel is
// all reach has done, and a stock sshd does not pass that on — a command that is
// not writing output never notices the pipe it would have got EPIPE from, so
// `sleep 600` survives, and so does a quiet build. Saying nothing leaves an
// operator believing a command stopped because reach stopped waiting for it,
// which is the sort of quiet wrong impression this project refuses elsewhere.
//
// A local target is different: reach owns that process and kills it outright.
func abandonedCommandNote(t Transport) string {
	if !mayOutliveDisconnect(t) {
		return ""
	}
	return "\n\nreach stopped waiting and closed the channel, but the command may still be\n" +
		"running on the target: a stock sshd offers no way to signal a remote process\n" +
		"group, and a command producing no output never notices the disconnect. Check\n" +
		"with `reach exec -- ps`, or give commands longer with `reach up --timeout`."
}

// mayOutliveDisconnect reports whether a command started through this transport
// can keep running after reach stops talking to it.
//
// A local command is reach's own child and is killed. Everything else runs
// under something reach only holds a pipe to — sshd, a container runtime — and
// closing the pipe is not the same as ending the process.
func mayOutliveDisconnect(t Transport) bool {
	_, local := t.(*LocalTransport)
	return !local
}

// announcePID has the remote shell print its own pid on stderr before the
// command starts, behind the run's random tag.
//
// sshd calls setsid for the session it starts, so that shell leads a process
// group holding everything the command spawns, and the pid names the group.
// That is what lets an abandoned command be ended rather than merely left: a
// stock sshd cannot signal it, but a second command on the same connection can.
func announcePID(tag string) string {
	return "printf '%s%s\\n' " + ShellQuote(tag) + " \"$$\" >&2; "
}

// signalRemoteGroup ends the process group a command was started in.
//
// SIGTERM first, so a build or a test runner can clean up; SIGKILL two seconds
// later from a detached subshell, so reach does not wait out the grace period.
// Where the shell turned out not to lead a group — some container runtimes do
// not call setsid — the group signal fails and the shell alone is signalled.
func signalRemoteGroup(t Transport, pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := strconv.Itoa(pid)
	cmd := "kill -TERM -- -" + p + " 2>/dev/null || kill -TERM " + p + " 2>/dev/null; " +
		"( sleep 2; kill -KILL -- -" + p + " 2>/dev/null || kill -KILL " + p + " 2>/dev/null ) </dev/null >/dev/null 2>&1 &"
	_, err := t.Run(ctx, reach.ExecRequest{Command: cmd})
	return err
}

func signalledNote(pid int) string {
	return fmt.Sprintf("\n\nreach stopped waiting and sent SIGTERM to the command's process group on the\n"+
		"target (pid %d), then SIGKILL two seconds later. A process that started its\n"+
		"own session (a daemon, setsid, nohup into the background) is not in that\n"+
		"group; check with `reach exec -- ps`.", pid)
}

// closeReader ends a blocked read on one of a stream's pipes, where the pipe can
// be closed at all.
func closeReader(r io.Reader) {
	if c, ok := r.(io.Closer); ok {
		_ = c.Close()
	}
}

// pidCapture takes the line announcePID printed off the front of a command's
// stderr and passes everything after it through untouched.
//
// Output is held only until the first newline, or until there is more of it
// than the tag line could be; a target that never printed the line (a shell
// that is not POSIX, a transport that does not run one) loses nothing.
type pidCapture struct {
	out  io.Writer
	tag  []byte
	buf  []byte
	done bool

	mu  sync.Mutex
	pid int
	ok  bool
}

func (c *pidCapture) Write(p []byte) (int, error) {
	if c.done {
		return c.out.Write(p)
	}
	c.buf = append(c.buf, p...)
	nl := bytes.IndexByte(c.buf, '\n')
	if nl < 0 && len(c.buf) < len(c.tag)+24 {
		return len(p), nil
	}
	c.done = true
	rest := c.buf
	if nl >= 0 && bytes.HasPrefix(c.buf, c.tag) {
		if n, err := strconv.Atoi(string(c.buf[len(c.tag):nl])); err == nil && n > 0 {
			c.mu.Lock()
			c.pid, c.ok = n, true
			c.mu.Unlock()
		}
		rest = c.buf[nl+1:]
	}
	c.buf = nil
	if len(rest) > 0 {
		if _, err := c.out.Write(rest); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// flush releases output held while waiting for a line that never came.
func (c *pidCapture) flush() {
	if !c.done && len(c.buf) > 0 {
		_, _ = c.out.Write(c.buf)
	}
	c.done, c.buf = true, nil
}

func (c *pidCapture) value() (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pid, c.ok
}

// complaint renders the target's own explanation as a suffix, when there is one.
func complaint(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return ": " + s
}

// countingWriter forwards writes and counts the bytes that got through.
type countingWriter struct {
	out io.Writer
	mu  sync.Mutex
	n   int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.mu.Lock()
	w.n += int64(n)
	w.mu.Unlock()
	return n, err
}

func (w *countingWriter) written() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// stderrTap forwards the target's stderr to the caller and keeps the first few
// kilobytes of it for reach's own diagnosis.
//
// The bound is because this holds whatever a remote command chooses to print,
// and a build log is not a diagnosis; the mutex is because the writer is an
// io.Copy in its own goroutine and the reader is the failure path.
type stderrTap struct {
	out       io.Writer
	mu        sync.Mutex
	kept      strings.Builder
	remaining int
}

func (w *stderrTap) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.remaining > 0 {
		keep := p
		if len(keep) > w.remaining {
			keep = keep[:w.remaining]
		}
		n, _ := w.kept.Write(keep)
		w.remaining -= n
	}
	w.mu.Unlock()
	return w.out.Write(p)
}

func (w *stderrTap) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.kept.String()
}

// SentinelFilter passes a streamed command's stdout through while holding back
// enough of the tail to recognise and remove the trailing status marker
// WrapWithSentinel adds. It is the streaming form of the marker recovery Run
// does internally, exported for the exec-server, which pumps output chunks to a
// client while the command is still running.
type SentinelFilter struct {
	inner *sentinelFilter
}

// NewSentinelFilter wraps out with a filter stripping "\n"+sentinel and the
// status digits that follow it.
func NewSentinelFilter(out io.Writer, sentinel string) *SentinelFilter {
	return &SentinelFilter{inner: &sentinelFilter{out: out, sentinel: []byte("\n" + sentinel)}}
}

// Write implements io.Writer.
func (f *SentinelFilter) Write(p []byte) (int, error) { return f.inner.Write(p) }

// Flush emits whatever remains, minus the status marker if it is present. It
// must be called once the command's stdout reaches EOF.
func (f *SentinelFilter) Flush() { f.inner.flush() }

// ExitCode returns the status the marker carried, and false when the marker
// never arrived — which means the transport, not the command, failed.
func (f *SentinelFilter) ExitCode() (int, bool) { return f.inner.exitCode() }

// sentinelFilter passes bytes through while holding back enough of the tail to
// recognise and remove the trailing status marker.
type sentinelFilter struct {
	out      io.Writer
	sentinel []byte
	pending  bytes.Buffer
	code     int
	haveCode bool
}

// holdFrom is where the output that might belong to the status marker begins;
// everything before it is the command's own and can go to the caller now.
//
// A fixed tail used to be withheld instead — the marker plus room for its
// digits, 67 bytes — and that was most of a short command's output: a tool
// call that timed out after printing "begin" showed the model nothing at all,
// where a local shell would have shown it the line. The marker starts with a
// newline and has none inside it, so the only output that can be the start of
// one is the last newline and what follows it, and only while that is still a
// prefix of the marker.
func (f *sentinelFilter) holdFrom(buf []byte) int {
	if i := bytes.Index(buf, f.sentinel); i >= 0 {
		return i
	}
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 && bytes.HasPrefix(f.sentinel, buf[i:]) {
		return i
	}
	return len(buf)
}

func (f *sentinelFilter) Write(p []byte) (int, error) {
	f.pending.Write(p)
	if emit := f.holdFrom(f.pending.Bytes()); emit > 0 {
		if _, err := f.out.Write(f.pending.Next(emit)); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// flush emits whatever remains, minus the status marker if it is present.
func (f *sentinelFilter) flush() {
	rest := f.pending.Bytes()
	if idx := bytes.LastIndex(rest, f.sentinel); idx >= 0 {
		tail := rest[idx+len(f.sentinel):]
		end := bytes.IndexByte(tail, '\n')
		if end < 0 {
			end = len(tail)
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(tail[:end]))); err == nil {
			f.code, f.haveCode = n, true
		}
		rest = rest[:idx]
	}
	if len(rest) > 0 {
		_, _ = f.out.Write(rest)
	}
	f.pending.Reset()
}

func (f *sentinelFilter) exitCode() (int, bool) { return f.code, f.haveCode }
