// Package cli implements the stable agent-facing command line interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/client"
	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/spf13/cobra"
)

type application struct {
	version                     string
	home                        string
	socket                      string
	jsonOutput                  bool
	stdin                       io.Reader
	stdout                      io.Writer
	stderr                      io.Writer
	validateSwapConfidentiality func() error
}

func NewRoot(version string, stdin io.Reader, stdout, stderr io.Writer) *cobra.Command {
	return newRoot(version, stdin, stdout, stderr, config.ValidateSwapConfidentiality)
}

func newRoot(version string, stdin io.Reader, stdout, stderr io.Writer, validateSwapConfidentiality func() error) *cobra.Command {
	a := &application{
		version: version, stdin: stdin, stdout: stdout, stderr: stderr,
		validateSwapConfidentiality: validateSwapConfidentiality,
	}
	root := &cobra.Command{
		Use:           "wechatcopilot",
		Short:         "Personal WeChat and WeCom control plane for local agents",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().StringVar(&a.home, "home", "", "absolute persistent state directory (or WECHATCOPILOT_HOME)")
	root.PersistentFlags().StringVar(&a.socket, "socket", "", "daemon Unix socket path")
	root.PersistentFlags().BoolVar(&a.jsonOutput, "json", false, "emit compact stable JSON")
	root.AddCommand(
		a.doctorCommand(),
		a.daemonCommand(),
		a.accountsCommand(),
		a.authCommand(),
		a.capabilitiesCommand(),
		a.conversationsCommand(),
		a.messagesCommand(),
		a.surfacesCommand(),
		a.mcpCommand(),
		a.versionCommand(),
	)
	return root
}

func Execute(ctx context.Context, version string) int {
	root := NewRoot(version, os.Stdin, os.Stdout, os.Stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		response, _ := api.Failure(err)
		if response.Error != nil && response.Error.Code == api.CodeInternal {
			response.Error.Message = err.Error()
		}
		_ = json.NewEncoder(os.Stdout).Encode(response)
		return 1
	}
	return 0
}

func (a *application) paths() (config.Paths, error) {
	paths, err := config.ResolvePaths()
	if err != nil {
		return config.Paths{}, err
	}
	paths, err = paths.WithHome(a.home)
	if err != nil {
		return config.Paths{}, err
	}
	if a.socket != "" {
		if !filepath.IsAbs(a.socket) {
			return config.Paths{}, errors.New("daemon socket path must be absolute")
		}
		paths.Socket = filepath.Clean(a.socket)
	}
	return paths, nil
}

func (a *application) daemonClient() (*client.Client, error) {
	paths, err := a.paths()
	if err != nil {
		return nil, err
	}
	return client.New(paths.Socket), nil
}

func (a *application) write(data any) error {
	return a.writeEnvelope(api.Success(data))
}

func (a *application) writeEnvelope(response api.Response) error {
	encoder := json.NewEncoder(a.stdout)
	encoder.SetEscapeHTML(false)
	if !a.jsonOutput {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(response)
}

func (a *application) writeJSONLine(data any) error {
	encoder := json.NewEncoder(a.stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(api.Success(data))
}

func invalidArgument(message string) error {
	return api.NewError(http.StatusBadRequest, api.CodeInvalidArgument, message)
}

func internalError(message string, err error) error {
	return api.WrapError(http.StatusInternalServerError, api.CodeInternal, message, err)
}

func requireValue(name, value string) error {
	if value == "" {
		return invalidArgument(fmt.Sprintf("--%s is required", name))
	}
	return nil
}

func noArgs(command *cobra.Command, args []string) error {
	if len(args) != 0 {
		return invalidArgument(fmt.Sprintf("%s does not accept positional arguments", command.CommandPath()))
	}
	return nil
}
