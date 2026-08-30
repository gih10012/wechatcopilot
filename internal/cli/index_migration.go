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
	"github.com/gih10012/wechatcopilot/internal/index"
	"github.com/spf13/cobra"
)

type legacyIndexAdoptionResult struct {
	AccountID    string          `json:"account_id"`
	AccountAlias string          `json:"account_alias"`
	Platform     driver.Platform `json:"platform"`
	Adopted      bool            `json:"adopted"`
}

func (a *application) accountAdoptLegacyIndexCommand() *cobra.Command {
	var accountReference string
	var confirm bool
	command := &cobra.Command{
		Use:   "adopt-legacy-index",
		Short: "Offline: bind an existing non-empty legacy message index to one saved account",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountReference); err != nil {
				return err
			}
			if !confirm {
				return invalidArgument("--confirm is required for legacy message index adoption")
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

			// Adoption must target an established registry. Refuse a wrong --home
			// before Paths.Ensure can initialize a new, empty state layout.
			if err := requireExistingAdoptionLayout(paths); err != nil {
				return api.WrapError(http.StatusConflict, api.CodeConflict, "registered state is unavailable for legacy index adoption", err)
			}
			if err := a.validateSwapConfidentiality(); err != nil {
				return api.WrapError(http.StatusServiceUnavailable, api.CodeDaemonUnavailable, "strict swap policy prevents legacy index adoption", err)
			}
			if err := paths.Ensure(); err != nil {
				return internalError("cannot validate private state directories", err)
			}

			stateLock, err := daemon.AcquireStateLock(paths.Home)
			if err != nil {
				if errors.Is(err, daemon.ErrStateLocked) {
					return api.WrapError(http.StatusConflict, api.CodeConflict, "stop the daemon before adopting a legacy message index", err)
				}
				return internalError("cannot lock daemon state home for legacy index adoption", err)
			}
			defer stateLock.Close()

			registry, err := account.Open(paths)
			if err != nil {
				return internalError("cannot open account registry for legacy index adoption", err)
			}
			item, err := registry.Resolve(accountReference)
			if errors.Is(err, os.ErrNotExist) {
				return api.NewError(http.StatusNotFound, api.CodeNotFound, "account not found")
			}
			if err != nil {
				return internalError("cannot resolve account for legacy index adoption", err)
			}
			if item.Deleting {
				return api.NewError(http.StatusConflict, api.CodeConflict, "account deletion is in progress")
			}

			indexPath := filepath.Join(paths.Accounts, item.ID, "index.sqlite3")
			migrated, err := index.AdoptLegacy(indexPath, item.ID)
			if err != nil {
				return api.WrapError(http.StatusConflict, api.CodeConflict, "legacy message index cannot be adopted", err)
			}
			if err := migrated.Close(); err != nil {
				return internalError("cannot close adopted message index", err)
			}

			return a.write(legacyIndexAdoptionResult{
				AccountID: item.ID, AccountAlias: item.Alias, Platform: item.Platform, Adopted: true,
			})
		},
	}
	command.Flags().StringVar(&accountReference, "account", "", "exact opaque account ID or unique alias")
	command.Flags().BoolVar(&confirm, "confirm", false, "required: bind this existing non-empty legacy index to the resolved saved account")
	return command
}

func requireExistingAdoptionLayout(paths config.Paths) error {
	for _, candidate := range []struct {
		path      string
		directory bool
	}{
		{path: paths.Home, directory: true},
		{path: paths.Accounts, directory: true},
		{path: paths.Registry},
	} {
		info, err := os.Lstat(candidate.path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("registered state path must not be a symbolic link")
		}
		if candidate.directory && !info.IsDir() {
			return errors.New("registered state directory is not a directory")
		}
		if !candidate.directory && !info.Mode().IsRegular() {
			return errors.New("account registry is not a regular file")
		}
	}
	return nil
}
