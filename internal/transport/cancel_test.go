package transport

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/agentreach/internal/reach"
)

// heldPipeTransport stands in for a multiplexed ssh: the command's output pipe
// is held open by someone other than the process reach started — the master
// connection — so killing that process does not end the read. It announces a
// pid the way a remote shell would, and records what reach runs afterwards.
type heldPipeTransport struct {
	mu   sync.Mutex
	runs []string
	keep []*os.File
}

func (h *heldPipeTransport) Run(_ context.Context, req reach.ExecRequest) (reach.ExecResult, error) {
	h.mu.Lock()
	h.runs = append(h.runs, req.Command)
	h.mu.Unlock()
	return reach.ExecResult{}, nil
}

func (h *heldPipeTransport) Open(_ context.Context, command string) (Stream, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return Stream{}, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return Stream{}, err
	}
	h.keep = append(h.keep, outW, errW)

	// The tag is random per run; read it back out of the wrapper the way the
	// remote shell would have expanded it.
	if i := strings.Index(command, "__reach_"); i >= 0 {
		if j := strings.Index(command[i:], "pid:"); j >= 0 {
			_, _ = io.WriteString(errW, command[i:i+j+len("pid:")]+"4242\nwarming up\n")
		}
	}
	_, _ = io.WriteString(outW, "first line of a long build\n"+strings.Repeat("x", 200)+"\n")
	return Stream{
		Stdin:  nopWriteCloser{io.Discard},
		Stdout: outR,
		Stderr: errR,
		Wait:   func() (int, error) { return 0, nil },
		Close:  func() error { return nil },
	}, nil
}

func (h *heldPipeTransport) Describe() string { return "ssh://held" }
func (h *heldPipeTransport) Close() error {
	for _, f := range h.keep {
		_ = f.Close()
	}
	return nil
}

// TestCancelDoesNotWaitForTheRemoteCommand: a multiplexed ssh hands its pipes to
// the master, so the output stays open for as long as the remote command runs.
// A timeout or a harness's SIGTERM has to end the call then, not when the
// command it gave up on finally exits.
func TestCancelDoesNotWaitForTheRemoteCommand(t *testing.T) {
	tr := &heldPipeTransport{}
	defer func() { _ = tr.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var out, errOut bytes.Buffer
	start := time.Now()
	_, err := RunStream(ctx, tr, reach.ExecRequest{Command: "make"}, &out, &errOut)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("RunStream returned %v after the deadline; it waited for the remote command", took)
	}
	if err == nil {
		t.Fatal("a cancelled command was reported as succeeding")
	}
	if !strings.Contains(out.String(), "first line of a long build") {
		t.Errorf("output streamed before the cancel was lost: %q", out.String())
	}
}

// TestCancelSignalsTheRemoteProcessGroup: the pid the remote shell announced
// names its process group, and reach ends that group instead of leaving the
// command running where nobody is waiting for it.
func TestCancelSignalsTheRemoteProcessGroup(t *testing.T) {
	tr := &heldPipeTransport{}
	defer func() { _ = tr.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var out, errOut bytes.Buffer
	_, err := RunStream(ctx, tr, reach.ExecRequest{Command: "sleep 600"}, &out, &errOut)
	if err == nil {
		t.Fatal("a cancelled command was reported as succeeding")
	}
	if len(tr.runs) != 1 || !strings.Contains(tr.runs[0], "kill -TERM -- -4242") {
		t.Fatalf("the remote process group was not signalled; ran %q", tr.runs)
	}
	if !strings.Contains(err.Error(), "sent SIGTERM") {
		t.Errorf("the error does not say the command was signalled: %v", err)
	}
	if strings.Contains(errOut.String(), "4242") {
		t.Errorf("the pid announcement leaked into the command's stderr: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "warming up") {
		t.Errorf("stderr after the announcement was lost: %q", errOut.String())
	}
}

func TestPIDCapture(t *testing.T) {
	const tag = "__reach_abc__pid:"
	for _, tc := range []struct {
		name   string
		chunks []string
		pid    int
		ok     bool
		out    string
	}{
		{"announced", []string{tag + "77\nboom\n"}, 77, true, "boom\n"},
		{"split across writes", []string{tag[:5], tag[5:] + "7", "7\nboom"}, 77, true, "boom"},
		{"never announced", []string{"bash: line 1: oops\n"}, 0, false, "bash: line 1: oops\n"},
		{"no newline, short", []string{"oops"}, 0, false, "oops"},
		{"spoofed by another tag", []string{"__reach_zzz__pid:9\n"}, 0, false, "__reach_zzz__pid:9\n"},
		{"after ssh's own warnings", []string{"** WARNING: connection is not using a post-quantum key exchange algorithm.\r\n** The server may need to be upgraded.\r\n" + tag + "77\nboom\n"}, 77, true,
			"** WARNING: connection is not using a post-quantum key exchange algorithm.\r\n** The server may need to be upgraded.\r\nboom\n"},
		{"warning split mid-line", []string{"** WARN", "ING\n" + tag, "5\n"}, 5, true, "** WARNING\n"},
		{"tag text mid-line is not the tag line", []string{"x" + tag + "5\n"}, 0, false, "x" + tag + "5\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := &pidCapture{out: &buf, tag: []byte(tag)}
			for _, ch := range tc.chunks {
				_, _ = c.Write([]byte(ch))
			}
			c.flush()
			pid, ok := c.value()
			if pid != tc.pid || ok != tc.ok {
				t.Errorf("pid = %d, %v; want %d, %v", pid, ok, tc.pid, tc.ok)
			}
			if buf.String() != tc.out {
				t.Errorf("passed through %q, want %q", buf.String(), tc.out)
			}
		})
	}
}

// TestSentinelFilterHoldsOnlyAPossibleMarker: output reaches the caller as soon
// as it cannot be the start of the status marker, so a short line printed by a
// command that later times out is not lost with it.
func TestSentinelFilterHoldsOnlyAPossibleMarker(t *testing.T) {
	const sentinel = "__reach_0123456789abcdef__"
	var out bytes.Buffer
	f := &sentinelFilter{out: &out, sentinel: []byte("\n" + sentinel)}

	_, _ = f.Write([]byte("begin"))
	if out.String() != "begin" {
		t.Fatalf("short output was withheld: %q", out.String())
	}
	// A newline might start the marker, so it waits for what follows it.
	_, _ = f.Write([]byte("\n"))
	_, _ = f.Write([]byte("__reach_ is a fine word\n"))
	if out.String() != "begin\n__reach_ is a fine word" {
		t.Fatalf("output that turned out not to be the marker was withheld: %q", out.String())
	}
	// The real marker, split across writes, is removed and its status kept.
	for _, chunk := range []string{"\n__reach_0123", "456789abcdef__", "7\n"} {
		_, _ = f.Write([]byte(chunk))
	}
	f.flush()
	if out.String() != "begin\n__reach_ is a fine word\n" {
		t.Errorf("the marker leaked or output was lost: %q", out.String())
	}
	if code, ok := f.exitCode(); !ok || code != 7 {
		t.Errorf("exit code = %d, %v; want 7, true", code, ok)
	}
}
