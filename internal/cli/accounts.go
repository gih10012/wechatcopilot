package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gih10012/wechatcopilot/internal/account"
	"github.com/gih10012/wechatcopilot/internal/auth"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func (a *application) accountsCommand() *cobra.Command {
	command := &cobra.Command{Use: "accounts", Short: "Create, switch, and inspect isolated saved accounts"}
	command.AddCommand(
		a.accountAddCommand(),
		a.accountListCommand(),
		a.accountStatusCommand(),
		a.accountLifecycleCommand("activate"),
		a.accountLifecycleCommand("deactivate"),
		a.accountRemoveCommand(),
		a.accountAdoptLegacyIndexCommand(),
		a.accountApproveLegacyWeComProfileCommand(),
		a.accountLoginCommand(),
	)
	return command
}

func (a *application) accountAddCommand() *cobra.Command {
	var alias string
	var platform string
	command := &cobra.Command{
		Use:   "add",
		Short: "Create an empty isolated profile for a new login",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("alias", alias); err != nil {
				return err
			}
			if platform != string(driver.PlatformWeChat) && platform != string(driver.PlatformWeCom) {
				return invalidArgument("--platform must be wechat or wecom")
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output account.Account
			if err := client.Post(command.Context(), "/v1/accounts/add", map[string]any{"alias": alias, "platform": platform}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&alias, "alias", "", "unique lowercase account alias")
	command.Flags().StringVar(&platform, "platform", "", "wechat or wecom")
	return command
}

func (a *application) accountListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved accounts",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output []account.Account
			if err := client.Get(command.Context(), "/v1/accounts", &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
}

func (a *application) accountStatusCommand() *cobra.Command {
	var accountID string
	command := &cobra.Command{
		Use:   "status",
		Short: "Read an account's official-client state",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output driver.Status
			if err := client.Post(command.Context(), "/v1/accounts/status", map[string]string{"account": accountID}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	return command
}

func (a *application) accountLifecycleCommand(operation string) *cobra.Command {
	var accountID string
	command := &cobra.Command{
		Use:   operation,
		Short: strings.ToUpper(operation[:1]) + operation[1:] + " one saved account runtime",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output account.Account
			if err := client.Post(command.Context(), "/v1/accounts/"+operation, map[string]string{"account": accountID}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	return command
}

func (a *application) accountRemoveCommand() *cobra.Command {
	var accountID string
	var purge bool
	var confirm bool
	command := &cobra.Command{
		Use:   "remove",
		Short: "Permanently delete a deactivated account and all local state",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if !confirm {
				return invalidArgument("--confirm is required for account removal")
			}
			if !purge {
				return invalidArgument("--purge is required for account removal in v0.1")
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output account.Account
			if err := client.Post(command.Context(), "/v1/accounts/remove", map[string]any{
				"account": accountID, "purge": purge, "confirmed": confirm,
			}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "exact opaque account ID")
	command.Flags().BoolVar(&purge, "purge", false, "required: permanently delete the local profile, runtime, and message index")
	command.Flags().BoolVar(&confirm, "confirm", false, "required: confirm permanent deletion of this exact account")
	return command
}

func (a *application) accountLoginCommand() *cobra.Command {
	var accountID string
	var lan bool
	var lanAddress string
	var wait bool
	command := &cobra.Command{
		Use:   "login",
		Short: "User-only: create a QR or phone verification challenge",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if lanAddress != "" && !lan {
				return invalidArgument("--lan-address requires --lan")
			}
			challenge, err := a.beginAuth(command, accountID, lan, lanAddress)
			if err != nil {
				return err
			}
			if !wait {
				return a.write(challenge)
			}
			_, _ = fmt.Fprintf(a.stderr, "Open %s\n", challengeURL(challenge))
			completed, err := a.waitAuth(command, challenge)
			if err != nil {
				return err
			}
			return a.write(completed)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().BoolVar(&lan, "lan", false, "bind the one-time page to the private LAN")
	command.Flags().StringVar(&lanAddress, "lan-address", "", "RFC1918 address assigned to an eligible local interface (requires --lan)")
	command.Flags().BoolVar(&wait, "wait", false, "wait until the user completes or expires the challenge")
	return command
}

func (a *application) authCommand() *cobra.Command {
	command := &cobra.Command{Use: "auth", Short: "User-only low-level login challenge commands", Hidden: true}
	command.AddCommand(a.authBeginCommand(), a.authStatusCommand(), a.authSubmitCommand(), a.authWaitCommand())
	return command
}

func (a *application) authBeginCommand() *cobra.Command {
	var accountID string
	var lan bool
	var lanAddress string
	command := &cobra.Command{
		Use:   "begin",
		Short: "Create a login challenge",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if lanAddress != "" && !lan {
				return invalidArgument("--lan-address requires --lan")
			}
			challenge, err := a.beginAuth(command, accountID, lan, lanAddress)
			if err != nil {
				return err
			}
			return a.write(challenge)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().BoolVar(&lan, "lan", false, "bind the one-time page to the private LAN")
	command.Flags().StringVar(&lanAddress, "lan-address", "", "RFC1918 address assigned to an eligible local interface (requires --lan)")
	return command
}

func (a *application) authStatusCommand() *cobra.Command {
	var challengeID string
	command := &cobra.Command{
		Use:   "status",
		Short: "Read a login challenge state",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("challenge", challengeID); err != nil {
				return err
			}
			challenge, err := a.authStatus(command, challengeID)
			if err != nil {
				return err
			}
			return a.write(challenge)
		},
	}
	command.Flags().StringVar(&challengeID, "challenge", "", "login challenge ID")
	return command
}

func (a *application) authWaitCommand() *cobra.Command {
	var challengeID string
	command := &cobra.Command{
		Use:   "wait",
		Short: "Wait for a login challenge to complete or expire",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("challenge", challengeID); err != nil {
				return err
			}
			initial, err := a.authStatus(command, challengeID)
			if err != nil {
				return err
			}
			completed, err := a.waitAuth(command, initial)
			if err != nil {
				return err
			}
			return a.write(completed)
		},
	}
	command.Flags().StringVar(&challengeID, "challenge", "", "login challenge ID")
	return command
}

func (a *application) authSubmitCommand() *cobra.Command {
	var challengeID string
	command := &cobra.Command{
		Use:   "submit-code",
		Short: "Read a phone verification code privately from the terminal or stdin",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("challenge", challengeID); err != nil {
				return err
			}
			code, err := a.readVerificationCode()
			if err != nil {
				return internalError("cannot read verification code", err)
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output map[string]bool
			if err := client.Post(command.Context(), "/v1/auth/submit", map[string]string{"id": challengeID, "code": code}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&challengeID, "challenge", "", "login challenge ID")
	return command
}

func (a *application) beginAuth(command *cobra.Command, accountID string, lan bool, lanAddress string) (auth.Challenge, error) {
	client, err := a.daemonClient()
	if err != nil {
		return auth.Challenge{}, err
	}
	var challenge auth.Challenge
	err = client.Post(command.Context(), "/v1/auth/begin", map[string]any{"account": accountID, "lan": lan, "lan_address": lanAddress}, &challenge)
	return challenge, err
}

func (a *application) authStatus(command *cobra.Command, challengeID string) (auth.Challenge, error) {
	client, err := a.daemonClient()
	if err != nil {
		return auth.Challenge{}, err
	}
	var challenge auth.Challenge
	err = client.Post(command.Context(), "/v1/auth/status", map[string]string{"id": challengeID}, &challenge)
	return challenge, err
}

func (a *application) waitAuth(command *cobra.Command, challenge auth.Challenge) (auth.Challenge, error) {
	for challenge.State != driver.StateOnline {
		if !challenge.ExpiresAt.IsZero() && time.Now().UTC().After(challenge.ExpiresAt) {
			return challenge, errors.New("authentication challenge expired")
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-command.Context().Done():
			timer.Stop()
			return challenge, command.Context().Err()
		case <-timer.C:
		}
		updated, err := a.authStatus(command, challenge.ID)
		if err != nil {
			return challenge, err
		}
		challenge = updated
	}
	return challenge, nil
}

func challengeURL(challenge auth.Challenge) string {
	if challenge.LANURL != "" {
		return challenge.LANURL
	}
	return challenge.LocalURL
}

func (a *application) readVerificationCode() (string, error) {
	if file, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		_, _ = fmt.Fprint(a.stderr, "Verification code: ")
		value, err := term.ReadPassword(int(file.Fd()))
		_, _ = fmt.Fprintln(a.stderr)
		return strings.TrimSpace(string(value)), err
	}
	reader := bufio.NewReader(io.LimitReader(a.stdin, 128))
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func (a *application) capabilitiesCommand() *cobra.Command {
	var accountID string
	command := &cobra.Command{
		Use:   "capabilities",
		Short: "Read version-specific driver support levels",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output map[string]driver.Support
			if err := client.Post(command.Context(), "/v1/capabilities", map[string]string{"account": accountID}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	return command
}
