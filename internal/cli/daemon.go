package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gih10012/wechatcopilot/internal/api"
	appconfig "github.com/gih10012/wechatcopilot/internal/app"
	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/daemon"
	"github.com/gih10012/wechatcopilot/internal/service"
	"github.com/spf13/cobra"
)

func (a *application) doctorCommand() *cobra.Command {
	var runtimeChecks bool
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Check state storage and official-client runtime prerequisites",
		Args:  noArgs,
		RunE: func(*cobra.Command, []string) error {
			paths, err := a.paths()
			if err != nil {
				return invalidArgument(err.Error())
			}
			checks := config.Doctor(paths, runtimeChecks)
			failed := false
			for _, check := range checks {
				failed = failed || !check.OK
			}
			return a.write(map[string]any{"ok": !failed, "checks": checks, "paths": paths})
		},
	}
	command.Flags().BoolVar(&runtimeChecks, "runtime", true, "also check Docker, Binder, and official-client artifacts")
	return command
}

func (a *application) daemonCommand() *cobra.Command {
	command := &cobra.Command{Use: "daemon", Short: "Run or manage the local Unix-socket daemon"}
	command.AddCommand(
		a.daemonServeCommand(),
		a.daemonHealthCommand(),
		a.daemonInstallCommand(),
		a.systemctlCommand("start"),
		a.systemctlCommand("stop"),
		a.systemctlCommand("restart"),
		a.systemctlCommand("status"),
	)
	return command
}

func (a *application) daemonServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve the local API in the foreground",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			paths, err := a.paths()
			if err != nil {
				return err
			}
			mountGuard, err := config.AcquireRequiredStateMount(paths.Home)
			if err != nil {
				return api.WrapError(http.StatusServiceUnavailable, api.CodeDaemonUnavailable, "required state volume is unavailable", err)
			}
			defer mountGuard.Close()
			if err := a.validateSwapConfidentiality(); err != nil {
				return api.WrapError(http.StatusServiceUnavailable, api.CodeDaemonUnavailable, "strict swap policy prevents daemon startup", err)
			}
			if err := paths.Ensure(); err != nil {
				return internalError("cannot initialize private state directories", err)
			}
			stateLock, err := daemon.AcquireStateLock(paths.Home)
			if err != nil {
				if errors.Is(err, daemon.ErrStateLocked) {
					return api.WrapError(http.StatusConflict, api.CodeConflict, "another daemon already owns this state home", err)
				}
				return internalError("cannot lock daemon state home", err)
			}
			defer stateLock.Close()
			control, err := service.New(paths, appconfig.DriverFactories(paths))
			if err != nil {
				return internalError("cannot initialize daemon service", err)
			}
			server := daemon.New(paths.Socket, control)
			if err := server.Listen(); err != nil {
				_ = control.Close(context.Background())
				return api.WrapError(http.StatusConflict, api.CodeConflict, "cannot listen on daemon socket", err)
			}
			for _, restoreErr := range control.Restore(command.Context()) {
				_, _ = fmt.Fprintf(a.stderr, "wechatcopilot: account restore failed: %v\n", restoreErr)
			}
			errCh := make(chan error, 1)
			go func() { errCh <- server.Serve() }()
			select {
			case err := <-errCh:
				if err != nil {
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					shutdownErr := server.Shutdown(shutdownCtx)
					return internalError("daemon stopped unexpectedly", errors.Join(err, shutdownErr))
				}
				return nil
			case <-command.Context().Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := server.Shutdown(shutdownCtx); err != nil {
					return internalError("daemon shutdown failed", err)
				}
				return nil
			}
		},
	}
}

func (a *application) daemonHealthCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Probe the running daemon",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output map[string]any
			if err := client.Get(command.Context(), "/v1/health", &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
}

func (a *application) daemonInstallCommand() *cobra.Command {
	var force bool
	var noStart bool
	command := &cobra.Command{
		Use:   "install",
		Short: "Install a hardened systemd user unit",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			paths, err := a.paths()
			if err != nil {
				return err
			}
			strictSwap, err := config.StrictSwapPolicy()
			if err != nil {
				return invalidArgument(err.Error())
			}
			configDir, err := os.UserConfigDir()
			if err != nil {
				return internalError("cannot locate user configuration directory", err)
			}
			unitPath := filepath.Join(configDir, "systemd", "user", "wechatcopilot.service")
			environmentPath := filepath.Join(configDir, "wechatcopilot", "environment")
			persistedSwapPolicyEnvironmentPath := filepath.Join(configDir, "wechatcopilot", "swap-policy.environment")
			persistedStateMountEnvironmentPath := filepath.Join(configDir, "wechatcopilot", "state-mount.environment")
			if _, err := os.Lstat(unitPath); err == nil && !force {
				return api.NewError(http.StatusConflict, api.CodeConflict, "systemd unit already exists; pass --force to replace it")
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			stateMountEnvironment, err := config.RequiredStateMountEnvironment()
			if err != nil {
				return api.WrapError(http.StatusServiceUnavailable, api.CodeDaemonUnavailable, "cannot persist required state mount", err)
			}
			if len(stateMountEnvironment) == 0 {
				persisted, err := config.HasPersistedStateMountGate()
				if err != nil {
					return internalError("cannot inspect the existing state mount gate", err)
				}
				if persisted {
					return api.NewError(http.StatusConflict, api.CodeConflict, "refusing to remove the existing state mount gate; unlock the volume and export all three WECHATCOPILOT_STATE_MOUNT_* variables before reinstalling")
				}
			}
			mountGuard, err := config.AcquireRequiredStateMount(paths.Home)
			if err != nil {
				return api.WrapError(http.StatusServiceUnavailable, api.CodeDaemonUnavailable, "required state volume is unavailable", err)
			}
			defer mountGuard.Close()
			if !noStart {
				if err := a.validateSwapConfidentiality(); err != nil {
					return api.WrapError(http.StatusServiceUnavailable, api.CodeDaemonUnavailable, "strict swap policy prevents daemon startup", err)
				}
			}
			if err := paths.Ensure(); err != nil {
				return internalError("cannot initialize private daemon directories", err)
			}
			binary, err := os.Executable()
			if err != nil {
				return internalError("cannot locate current executable", err)
			}
			binary, err = filepath.EvalSymlinks(binary)
			if err != nil {
				return internalError("cannot resolve current executable", err)
			}
			stateMountEnvironmentPath := ""
			if len(stateMountEnvironment) > 0 {
				stateMountEnvironmentPath = persistedStateMountEnvironmentPath
				contents := []byte(strings.Join(stateMountEnvironment, "\n") + "\n")
				if err := config.AtomicWrite(stateMountEnvironmentPath, contents, 0o600); err != nil {
					return internalError("cannot write required state mount environment", err)
				}
			}
			swapPolicyContents := []byte(config.EnvStrictSwap + "=" + strconv.FormatBool(strictSwap) + "\n")
			if err := config.AtomicWrite(persistedSwapPolicyEnvironmentPath, swapPolicyContents, 0o600); err != nil {
				return internalError("cannot write required swap policy environment", err)
			}
			unit := systemdUnit(binary, paths.Home, environmentPath, persistedSwapPolicyEnvironmentPath, stateMountEnvironmentPath)
			if err := config.AtomicWrite(unitPath, []byte(unit), 0o600); err != nil {
				return internalError("cannot write systemd user unit", err)
			}
			if _, err := runSystemctl(command.Context(), "daemon-reload"); err != nil {
				return err
			}
			var systemctlOutput []string
			output, err := runSystemctl(command.Context(), "enable", "wechatcopilot.service")
			if err != nil {
				return err
			}
			if output != "" {
				systemctlOutput = append(systemctlOutput, output)
			}
			if !noStart {
				output, err = runSystemctl(command.Context(), "restart", "wechatcopilot.service")
				if err != nil {
					return err
				}
				if output != "" {
					systemctlOutput = append(systemctlOutput, output)
				}
			}
			return a.write(map[string]any{
				"unit": unitPath, "environment_file": environmentPath,
				"swap_policy_environment_file": persistedSwapPolicyEnvironmentPath,
				"state_mount_environment_file": stateMountEnvironmentPath,
				"strict_swap":                  strictSwap,
				"started":                      !noStart, "systemctl": strings.Join(systemctlOutput, "\n"),
			})
		},
	}
	command.Flags().BoolVar(&force, "force", false, "replace an existing wechatcopilot user unit")
	command.Flags().BoolVar(&noStart, "no-start", false, "enable the unit without starting it")
	return command
}

func (a *application) systemctlCommand(operation string) *cobra.Command {
	return &cobra.Command{
		Use:   operation,
		Short: strings.ToUpper(operation[:1]) + operation[1:] + " the systemd user service",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			args := []string{operation, "wechatcopilot.service"}
			if operation == "status" {
				args = []string{"show", "wechatcopilot.service", "--property=ActiveState,SubState,MainPID,ExecMainStatus"}
			}
			output, err := runSystemctl(command.Context(), args...)
			if err != nil {
				return err
			}
			return a.write(map[string]any{"operation": operation, "output": output})
		},
	}
}

func runSystemctl(ctx context.Context, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	output, err := command.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		return text, api.WrapError(http.StatusServiceUnavailable, api.CodeDaemonUnavailable, "systemd user service operation failed", fmt.Errorf("%w: %s", err, text))
	}
	return text, nil
}

func systemdUnit(binary, stateHome, environmentPath, swapPolicyEnvironmentPath, stateMountEnvironmentPath string) string {
	binary = strings.ReplaceAll(binary, "%", "%%")
	stateHome = strings.ReplaceAll(stateHome, "%", "%%")
	environmentPath = strings.ReplaceAll(environmentPath, "%", "%%")
	swapPolicyEnvironmentPath = strings.ReplaceAll(swapPolicyEnvironmentPath, "%", "%%")
	stateMountEnvironmentPath = strings.ReplaceAll(stateMountEnvironmentPath, "%", "%%")
	lines := []string{
		"[Unit]",
		"Description=WeChat Copilot local daemon",
		"After=default.target docker.service",
		"",
		"[Service]",
		"Type=simple",
		"Environment=" + strconv.Quote("WECHATCOPILOT_HOME="+stateHome),
		"EnvironmentFile=-" + strconv.Quote(environmentPath),
		"EnvironmentFile=" + strconv.Quote(swapPolicyEnvironmentPath),
	}
	if stateMountEnvironmentPath != "" {
		lines = append(lines, "EnvironmentFile="+strconv.Quote(stateMountEnvironmentPath))
	}
	lines = append(lines,
		"ExecStart="+strconv.Quote(binary)+" daemon serve",
		"Restart=on-failure",
		"RestartSec=3",
		"TimeoutStopSec=40",
		"UMask=0077",
		"NoNewPrivileges=yes",
		"PrivateTmp=yes",
		"ProtectSystem=strict",
		"ProtectHome=read-only",
		"RuntimeDirectory=wechatcopilot",
		"RuntimeDirectoryMode=0700",
		"ReadWritePaths="+strconv.Quote(stateHome),
		"ReadWritePaths=%t/wechatcopilot",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	)
	return strings.Join(lines, "\n")
}
