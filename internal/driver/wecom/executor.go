package wecom

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// Executor exists so orchestration can be tested without Docker or a
// privileged Android runtime.
type Executor interface {
	Run(context.Context, string, ...string) ([]byte, error)
	RunInput(context.Context, []byte, int64, string, ...string) ([]byte, error)
}

type OSExecutor struct{}

func (OSExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	stdout := &boundedBuffer{remaining: 64 << 20}
	stderr := &boundedBuffer{remaining: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s failed: %w: %s", name, err, stderr.String())
	}
	if stdout.overflow {
		return nil, fmt.Errorf("%s output exceeded 64 MiB", name)
	}
	return stdout.Bytes(), nil
}

func (OSExecutor) RunInput(ctx context.Context, input []byte, maxOutputBytes int64, name string, args ...string) ([]byte, error) {
	if maxOutputBytes <= 0 {
		return nil, errors.New("maximum command output must be positive")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	stdout := &boundedBuffer{remaining: maxOutputBytes}
	stderr := &boundedBuffer{remaining: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		// Interactive stdin may contain the companion bearer token. A child
		// can reflect stdin to stderr, so never surface stderr from this path.
		return nil, fmt.Errorf("%s interactive command failed: %w", name, err)
	}
	if stdout.overflow {
		return nil, fmt.Errorf("%s output exceeded %d bytes", name, maxOutputBytes)
	}
	return stdout.Bytes(), nil
}

// boundedBuffer keeps a child process from growing daemon memory without
// forcing an early pipe close that could hide the command's real exit status.
type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int64
	overflow  bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if int64(len(p)) > b.remaining {
		p = p[:max(b.remaining, 0)]
		b.overflow = true
	}
	if len(p) > 0 {
		if _, err := b.buffer.Write(p); err != nil {
			return 0, err
		}
		b.remaining -= int64(len(p))
	}
	return original, nil
}

func (b *boundedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *boundedBuffer) String() string { return b.buffer.String() }

var _ io.Writer = (*boundedBuffer)(nil)
