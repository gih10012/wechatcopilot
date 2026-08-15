package cli

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/service"
	"github.com/spf13/cobra"
)

func (a *application) conversationsCommand() *cobra.Command {
	command := &cobra.Command{Use: "conversations", Short: "Resolve opaque conversation targets"}
	command.AddCommand(a.conversationsListCommand(false), a.conversationsListCommand(true))
	return command
}

func (a *application) conversationsListCommand(searchMode bool) *cobra.Command {
	var accountID string
	var query string
	var unread bool
	var limit int
	name := "list"
	short := "List locally indexed conversations"
	if searchMode {
		name = "search"
		short = "Search locally indexed conversation titles"
	}
	command := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if searchMode {
				if err := requireValue("query", query); err != nil {
					return err
				}
			}
			if limit < 0 || limit > 500 {
				return invalidArgument("--limit must be between 0 and 500")
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output []driver.Conversation
			if err := client.Post(command.Context(), "/v1/conversations/list", map[string]any{
				"account": accountID, "search": query, "unread": unread, "limit": limit,
			}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&query, "query", "", "case-insensitive title substring")
	command.Flags().BoolVar(&unread, "unread", false, "only return conversations with unread messages")
	command.Flags().IntVar(&limit, "limit", 100, "maximum conversations")
	return command
}

func (a *application) messagesCommand() *cobra.Command {
	command := &cobra.Command{Use: "messages", Short: "Read, search, watch, and transactionally send messages"}
	command.AddCommand(
		a.messagesHistoryCommand(),
		a.messagesWatchCommand(),
		a.messagesSearchCommand(),
		a.messagesPrepareCommand(),
		a.messagesCommitCommand(),
	)
	return command
}

func (a *application) messagesHistoryCommand() *cobra.Command {
	var accountID string
	var conversationID string
	var cursor int64
	var limit int
	var latest bool
	command := &cobra.Command{
		Use:     "history",
		Aliases: []string{"list"},
		Short:   "Read locally indexed messages in sequence order",
		Args:    noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if limit < 0 || limit > 1000 {
				return invalidArgument("--limit must be between 0 and 1000")
			}
			if latest && cursor != 0 {
				return invalidArgument("--latest cannot be combined with a nonzero --cursor")
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output []driver.Message
			if err := client.Post(command.Context(), "/v1/messages/list", map[string]any{
				"account": accountID, "conversation_id": conversationID,
				"after_sequence": cursor, "limit": limit, "latest": latest,
			}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&conversationID, "conversation", "", "opaque conversation ID")
	command.Flags().Int64Var(&cursor, "cursor", 0, "only return messages after this local sequence")
	command.Flags().IntVar(&limit, "limit", 100, "maximum messages")
	command.Flags().BoolVar(&latest, "latest", false, "return the latest matching messages in sequence order")
	return command
}

func (a *application) messagesWatchCommand() *cobra.Command {
	var accountID string
	var cursor int64
	var timeout time.Duration
	var limit int
	var follow bool
	var jsonLines bool
	command := &cobra.Command{
		Use:   "watch",
		Short: "Poll for messages after a local sequence cursor",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if timeout <= 0 || timeout > 30*time.Second {
				return invalidArgument("--timeout must be greater than zero and at most 30s")
			}
			if limit < 1 || limit > 1000 {
				return invalidArgument("--limit must be between 1 and 1000")
			}
			if follow && !jsonLines {
				return invalidArgument("--follow requires --jsonl")
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			for {
				var output []driver.Message
				err := client.Post(command.Context(), "/v1/messages/watch", map[string]any{
					"account": accountID, "after_sequence": cursor,
					"timeout_ms": timeout.Milliseconds(), "limit": limit,
				}, &output)
				if err != nil {
					return err
				}
				if !follow {
					return a.write(output)
				}
				for _, message := range output {
					if err := a.writeJSONLine(message); err != nil {
						return err
					}
					if message.Sequence > cursor {
						cursor = message.Sequence
					}
				}
				select {
				case <-command.Context().Done():
					return nil
				default:
				}
			}
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().Int64Var(&cursor, "cursor", 0, "last processed local sequence")
	command.Flags().DurationVar(&timeout, "timeout", 25*time.Second, "bounded duration for each poll")
	command.Flags().IntVar(&limit, "limit", 100, "maximum messages per poll")
	command.Flags().BoolVar(&follow, "follow", false, "continue polling until interrupted")
	command.Flags().BoolVar(&jsonLines, "jsonl", false, "emit one JSON envelope per message")
	return command
}

func (a *application) messagesSearchCommand() *cobra.Command {
	var accountID string
	var text string
	var limit int
	command := &cobra.Command{
		Use:   "search",
		Short: "Full-text search the local message index",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if err := requireValue("text", text); err != nil {
				return err
			}
			if limit < 1 || limit > 500 {
				return invalidArgument("--limit must be between 1 and 500")
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output []driver.Message
			if err := client.Post(command.Context(), "/v1/messages/search", map[string]any{
				"account": accountID, "text": text, "limit": limit,
			}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&text, "text", "", "FTS search expression")
	command.Flags().IntVar(&limit, "limit", 100, "maximum matches")
	return command
}

func (a *application) messagesPrepareCommand() *cobra.Command {
	var accountID string
	var conversationID string
	var text string
	var textFile string
	var attachmentPaths []string
	var shareSurfaceID string
	command := &cobra.Command{
		Use:   "prepare-send",
		Short: "Create a five-minute exact send preview without sending",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if err := requireValue("conversation", conversationID); err != nil {
				return err
			}
			body, err := a.resolveText(text, textFile)
			if err != nil {
				return err
			}
			attachments, err := resolveAttachments(attachmentPaths)
			if err != nil {
				return err
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output service.PreparedSend
			if err := client.Post(command.Context(), "/v1/messages/prepare-send", map[string]any{
				"account": accountID, "conversation_id": conversationID, "text": body,
				"attachments": attachments, "share_surface_id": shareSurfaceID,
			}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&conversationID, "conversation", "", "exact opaque conversation ID")
	command.Flags().StringVar(&text, "text", "", "message text")
	command.Flags().StringVar(&textFile, "text-file", "", "read message text from a file, or - for stdin")
	command.Flags().StringSliceVar(&attachmentPaths, "attachment", nil, "local attachment path (repeatable)")
	command.Flags().StringVar(&shareSurfaceID, "share-surface", "", "open surface ID to share")
	return command
}

func (a *application) messagesCommitCommand() *cobra.Command {
	var transactionID string
	var idempotencyKey string
	var confirm bool
	command := &cobra.Command{
		Use:   "commit-send",
		Short: "Send one authorized prepared transaction",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("transaction", transactionID); err != nil {
				return err
			}
			if err := requireValue("idempotency-key", idempotencyKey); err != nil {
				return err
			}
			if !confirm {
				return invalidArgument("--confirm is required to send")
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output driver.SendResult
			if err := client.Post(command.Context(), "/v1/messages/commit-send", map[string]any{
				"transaction_id": transactionID, "idempotency_key": idempotencyKey, "confirmed": confirm,
			}, &output); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&transactionID, "transaction", "", "prepared transaction ID")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "unique key for this exact logical send")
	command.Flags().BoolVar(&confirm, "confirm", false, "confirm the exact prepared preview")
	return command
}

func (a *application) resolveText(inline, source string) (string, error) {
	if inline != "" && source != "" {
		return "", invalidArgument("use only one of --text and --text-file")
	}
	if source == "" {
		return inline, nil
	}
	var reader io.Reader
	var file *os.File
	if source == "-" {
		reader = a.stdin
	} else {
		var err error
		file, err = os.Open(source)
		if err != nil {
			return "", invalidArgument("cannot open --text-file: " + err.Error())
		}
		defer file.Close()
		reader = file
	}
	contents, err := io.ReadAll(io.LimitReader(reader, 1<<20+1))
	if err != nil {
		return "", internalError("cannot read message text", err)
	}
	if len(contents) > 1<<20 {
		return "", invalidArgument("message text is larger than 1 MiB")
	}
	return string(contents), nil
}

func resolveAttachments(paths []string) ([]driver.Attachment, error) {
	if len(paths) > 20 {
		return nil, invalidArgument("at most 20 attachments are allowed")
	}
	attachments := make([]driver.Attachment, 0, len(paths))
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, invalidArgument("cannot resolve attachment path: " + err.Error())
		}
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, invalidArgument("cannot inspect attachment: " + err.Error())
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, invalidArgument("attachment must be a regular, non-symlink file: " + absolute)
		}
		attachments = append(attachments, driver.Attachment{
			Kind: "file", Name: filepath.Base(absolute), Size: info.Size(), LocalPath: absolute,
		})
	}
	return attachments, nil
}
