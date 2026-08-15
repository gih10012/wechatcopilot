package cli

import (
	"context"

	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/mcpserver"
	"github.com/gih10012/wechatcopilot/internal/service"
	"github.com/spf13/cobra"
)

type surfaceOutput struct {
	Surface          driver.Surface `json:"surface"`
	ScreenshotBase64 string         `json:"screenshot_base64,omitempty"`
}

func (a *application) surfacesCommand() *cobra.Command {
	command := &cobra.Command{Use: "surfaces", Short: "Open and semantically operate webpages and mini programs"}
	command.AddCommand(
		a.surfaceOpenCommand(),
		a.surfaceSnapshotCommand(false),
		a.surfaceSnapshotCommand(true),
		a.surfaceActCommand(false),
		a.surfaceActCommand(true),
		a.surfaceCloseCommand(),
		a.surfaceShareCommand(),
	)
	return command
}

func (a *application) surfaceOpenCommand() *cobra.Command {
	var accountID string
	var reference string
	command := &cobra.Command{
		Use:   "open",
		Short: "Open a message-backed webpage or mini-program reference",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if err := requireValue("ref", reference); err != nil {
				return err
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output surfaceOutput
			if err := client.Post(command.Context(), "/v1/surfaces/open", map[string]string{
				"account": accountID, "ref": reference,
			}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&reference, "ref", "", "surface reference returned by a message")
	return command
}

func (a *application) surfaceSnapshotCommand(actionsOnly bool) *cobra.Command {
	var accountID string
	var surfaceID string
	name := "snapshot"
	short := "Capture current semantic surface state"
	if actionsOnly {
		name = "actions"
		short = "List allowed semantic actions from the current surface"
	}
	command := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if err := requireValue("surface", surfaceID); err != nil {
				return err
			}
			output, err := a.snapshotSurface(command.Context(), accountID, surfaceID)
			if err != nil {
				return err
			}
			if actionsOnly {
				return a.write(map[string]any{
					"surface_id": output.Surface.ID, "actions": output.Surface.Actions,
					"observed_at": output.Surface.ObservedAt,
				})
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&surfaceID, "surface", "", "open surface ID")
	return command
}

func (a *application) surfaceActCommand(back bool) *cobra.Command {
	var accountID string
	var surfaceID string
	var actionID string
	var text string
	name := "act"
	short := "Use one semantic action from the latest snapshot"
	if back {
		name = "back"
		short = "Use the surface's semantic back action"
	}
	command := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if err := requireValue("surface", surfaceID); err != nil {
				return err
			}
			if back {
				snapshot, err := a.snapshotSurface(command.Context(), accountID, surfaceID)
				if err != nil {
					return err
				}
				for _, action := range snapshot.Surface.Actions {
					if action.Kind != "back" || action.Disabled || action.Risk == "high" {
						continue
					}
					if actionID != "" {
						return invalidArgument("surface exposes more than one back action; use surfaces act with an exact action ID")
					}
					actionID = action.ID
				}
				if actionID == "" {
					return invalidArgument("surface does not expose a safe semantic back action")
				}
			}
			if err := requireValue("action", actionID); err != nil {
				return err
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output surfaceOutput
			if err := client.Post(command.Context(), "/v1/surfaces/act", map[string]string{
				"account": accountID, "id": surfaceID, "action_id": actionID, "text": text,
			}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&surfaceID, "surface", "", "open surface ID")
	if !back {
		command.Flags().StringVar(&actionID, "action", "", "semantic action ID from the latest snapshot")
		command.Flags().StringVar(&text, "text", "", "text for an input action")
	}
	return command
}

func (a *application) surfaceCloseCommand() *cobra.Command {
	var accountID string
	var surfaceID string
	command := &cobra.Command{
		Use:   "close",
		Short: "Close an open surface",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if err := requireValue("surface", surfaceID); err != nil {
				return err
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output map[string]bool
			if err := client.Post(command.Context(), "/v1/surfaces/close", map[string]string{
				"account": accountID, "id": surfaceID,
			}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&surfaceID, "surface", "", "open surface ID")
	return command
}

func (a *application) surfaceShareCommand() *cobra.Command {
	var accountID string
	var surfaceID string
	var conversationID string
	command := &cobra.Command{
		Use:   "share",
		Short: "Prepare sharing a surface into an exact conversation",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if err := requireValue("surface", surfaceID); err != nil {
				return err
			}
			if err := requireValue("conversation", conversationID); err != nil {
				return err
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output service.PreparedSend
			if err := client.Post(command.Context(), "/v1/messages/prepare-send", map[string]string{
				"account": accountID, "conversation_id": conversationID, "share_surface_id": surfaceID,
			}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&surfaceID, "surface", "", "open surface ID")
	command.Flags().StringVar(&conversationID, "conversation", "", "exact opaque destination conversation ID")
	return command
}

func (a *application) snapshotSurface(ctx context.Context, accountID, surfaceID string) (surfaceOutput, error) {
	client, err := a.daemonClient()
	if err != nil {
		return surfaceOutput{}, err
	}
	var output surfaceOutput
	err = client.Post(ctx, "/v1/surfaces/snapshot", map[string]string{"account": accountID, "id": surfaceID}, &output)
	return output, err
}

func (a *application) mcpCommand() *cobra.Command {
	command := &cobra.Command{Use: "mcp", Short: "Expose the daemon through Model Context Protocol"}
	command.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Serve MCP over stdin/stdout",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			paths, err := a.paths()
			if err != nil {
				return err
			}
			return mcpserver.New(paths.Socket, a.version).Run(command.Context())
		},
	})
	return command
}

func (a *application) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build and API versions",
		Args:  noArgs,
		RunE: func(*cobra.Command, []string) error {
			return a.write(map[string]string{"version": a.version, "api_schema": "1"})
		},
	}
}
