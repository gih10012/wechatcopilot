package cli

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gih10012/wechatcopilot/internal/account"
	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/daemon"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/driver/wecom"
	"github.com/spf13/cobra"
)

type legacyWeComProfileApprovalResult struct {
	AccountID    string          `json:"account_id"`
	AccountAlias string          `json:"account_alias"`
	Platform     driver.Platform `json:"platform"`
	Approved     bool            `json:"approved"`
}

func (a *application) accountApproveLegacyWeComProfileCommand() *cobra.Command {
	var accountReference string
	var confirm bool
	command := &cobra.Command{
		Use:   "approve-legacy-wecom-profile",
		Short: "Offline: approve one stopped legacy WeCom profile missing external metadata",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountReference); err != nil {
				return err
			}
			// Confirmation is checked before paths are resolved or touched. An
			// agent cannot turn a probe of this command into a state mutation.
			if !confirm {
				return invalidArgument("--confirm is required for legacy WeCom profile approval")
			}

			paths, err := a.paths()
			if err != nil {
				return invalidArgument(err.Error())
			}
			mountGuard, err := config.AcquireRequiredStateMount(paths.Home)
			if err != nil {
				return api.WrapError(http.StatusServiceUnavailable, api.CodeDaemonUnavailable, "required state volume is unavailable", err)
			}
			defer mountGuard.Close()

			// Require the established registry before Paths.Ensure or account.Open
			// can initialize an empty layout under a mistyped --home.
			if err := requireExistingAdoptionLayout(paths); err != nil {
				return api.WrapError(http.StatusConflict, api.CodeConflict, "registered state is unavailable for legacy WeCom profile approval", err)
			}
			if err := a.validateSwapConfidentiality(); err != nil {
				return api.WrapError(http.StatusServiceUnavailable, api.CodeDaemonUnavailable, "strict swap policy prevents legacy WeCom profile approval", err)
			}
			if err := paths.Ensure(); err != nil {
				return internalError("cannot validate private state directories", err)
			}

			stateLock, err := daemon.AcquireStateLock(paths.Home)
			if err != nil {
				if errors.Is(err, daemon.ErrStateLocked) {
					return api.WrapError(http.StatusConflict, api.CodeConflict, "stop the daemon before approving a legacy WeCom profile", err)
				}
				return internalError("cannot lock daemon state home for legacy WeCom profile approval", err)
			}
			defer stateLock.Close()

			registry, err := account.Open(paths)
			if err != nil {
				return internalError("cannot open account registry for legacy WeCom profile approval", err)
			}
			item, err := registry.Resolve(accountReference)
			if errors.Is(err, os.ErrNotExist) {
				return api.NewError(http.StatusNotFound, api.CodeNotFound, "account not found")
			}
			if err != nil {
				return internalError("cannot resolve account for legacy WeCom profile approval", err)
			}
			if item.Deleting {
				return api.NewError(http.StatusConflict, api.CodeConflict, "account deletion is in progress")
			}
			if item.Platform != driver.PlatformWeCom {
				return api.NewError(http.StatusConflict, api.CodeConflict, "account is not a WeCom account")
			}

			stateDir := filepath.Join(paths.Accounts, item.ID)
			dockerBinary := os.Getenv(config.EnvDocker)
			if dockerBinary == "" {
				dockerBinary = "docker"
			}
			if err := wecom.CreateStoppedLegacyProfileApproval(
				command.Context(), stateDir, item.ID,
				wecom.LegacyProfileApprovalConfig{
					DockerBinary: dockerBinary,
					RedroidImage: os.Getenv(config.EnvWeComRedroidImage),
					Executor:     a.legacyWeComExecutor,
				},
			); err != nil {
				return api.WrapError(http.StatusConflict, api.CodeConflict, "legacy WeCom profile cannot be approved", err)
			}
			return a.write(legacyWeComProfileApprovalResult{
				AccountID: item.ID, AccountAlias: item.Alias, Platform: item.Platform, Approved: true,
			})
		},
	}
	command.Flags().StringVar(&accountReference, "account", "", "exact opaque account ID or unique alias")
	command.Flags().BoolVar(&confirm, "confirm", false, "required: approve this account's existing markerless WeCom data inode for one-time migration")
	return command
}
