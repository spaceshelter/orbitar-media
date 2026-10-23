// Package proc runs external tools (ffmpeg, ffprobe, vips, file) with a wall-clock limit and an
// address-space cap, so a pathological input can neither run forever nor eat the host's memory.
package proc

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Limits for one run.
type Limits struct {
	Timeout  time.Duration
	MemoryMB int // ulimit -v; 0 = no cap
}

// Result of a run: stdout, the tail of stderr and the error (nil on exit 0).
type Result struct {
	Stdout []byte
	Stderr string
	Err    error
}

// Run executes name with args. The command is started through /bin/sh only to apply
// `ulimit -v`; the arguments are passed as "$@" and never interpreted by the shell.
func Run(ctx context.Context, lim Limits, name string, args ...string) Result {
	if lim.Timeout <= 0 {
		lim.Timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	var cmd *exec.Cmd
	if lim.MemoryMB > 0 {
		script := "ulimit -v " + strconv.Itoa(lim.MemoryMB*1024) + " && exec \"$@\""
		cmd = exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", script, "sh", name}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, name, args...)
	}
	cmd.WaitDelay = 5 * time.Second
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: 4096}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("killed after %s: %w", lim.Timeout, err)
	}
	return Result{Stdout: stdout.Bytes(), Stderr: strings.TrimSpace(stderr.String()), Err: err}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
