package wechat

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

type Command struct {
	Name  string
	Args  []string
	Stdin []byte
}

// CommandRunner exists so the Docker boundary can be tested without starting
// the official client or granting a test process access to the Docker socket.
type CommandRunner interface {
	Run(context.Context, Command) ([]byte, error)
}

type ExecCommandRunner struct{}

func (ExecCommandRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.Stdin = bytes.NewReader(command.Stdin)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Stdin can contain an authentication code or message text. It is never
		// included in the returned error.
		return nil, fmt.Errorf("run %s: %w: %s", command.Name, err, truncate(stderr.String(), 512))
	}
	return stdout.Bytes(), nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
