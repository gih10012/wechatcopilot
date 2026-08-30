package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/mcpserver"
	"github.com/gih10012/wechatcopilot/internal/service"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

type surfaceOutput struct {
	Surface          driver.Surface `json:"surface"`
	ScreenshotBase64 string         `json:"screenshot_base64,omitempty"`
	ScreenshotPath   string         `json:"screenshot_path,omitempty"`
}

type surfaceExportOutput struct {
	Asset      driver.SurfaceAssetExport `json:"asset"`
	DataBase64 string                    `json:"data_base64,omitempty"`
}

func (a *application) surfacesCommand() *cobra.Command {
	command := &cobra.Command{Use: "surfaces", Short: "Open and semantically operate webpages and mini programs"}
	command.AddCommand(
		a.surfaceOpenCommand(),
		a.surfaceSnapshotCommand(false),
		a.surfaceSnapshotCommand(true),
		a.surfaceActCommand(false),
		a.surfaceActCommand(true),
		a.surfaceExportCommand(),
		a.surfaceCloseCommand(),
		a.surfaceShareCommand(),
	)
	return command
}

func (a *application) surfaceOpenCommand() *cobra.Command {
	var accountID string
	var reference string
	var miniProgram string
	var screenshotOut string
	var withoutImageData bool
	command := &cobra.Command{
		Use:   "open",
		Short: "Open a message-backed surface or a mini program by name",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := requireValue("account", accountID); err != nil {
				return err
			}
			if (reference == "") == (miniProgram == "") {
				return invalidArgument("exactly one of --ref or --mini-program is required")
			}
			screenshotFile, err := reserveOptionalPrivateFile(screenshotOut)
			if err != nil {
				return err
			}
			if screenshotFile != nil {
				defer screenshotFile.discard()
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output surfaceOutput
			if err := client.Post(command.Context(), "/v1/surfaces/open", map[string]any{
				"account": accountID, "ref": reference, "mini_program": miniProgram,
				"without_image_data": withoutImageData && screenshotFile == nil,
			}, &output); err != nil {
				return err
			}
			if err := finalizeSurfaceOutput(&output, screenshotFile, withoutImageData); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&reference, "ref", "", "surface reference returned by a message")
	command.Flags().StringVar(&miniProgram, "mini-program", "", "exact mini-program display name")
	command.Flags().StringVar(&screenshotOut, "screenshot-out", "", "securely create this file with the matching PNG screenshot")
	command.Flags().BoolVar(&withoutImageData, "without-image-data", false, "omit screenshot base64 from JSON output")
	return command
}

func (a *application) surfaceSnapshotCommand(actionsOnly bool) *cobra.Command {
	var accountID string
	var surfaceID string
	var screenshotOut string
	var withoutImageData bool
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
			screenshotFile, err := reserveOptionalPrivateFile(screenshotOut)
			if err != nil {
				return err
			}
			if screenshotFile != nil {
				defer screenshotFile.discard()
			}
			output, err := a.snapshotSurface(command.Context(), accountID, surfaceID, actionsOnly || (withoutImageData && screenshotFile == nil))
			if err != nil {
				return err
			}
			if actionsOnly {
				return a.write(map[string]any{
					"surface_id": output.Surface.ID, "actions": output.Surface.Actions,
					"observed_at": output.Surface.ObservedAt,
				})
			}
			if err := finalizeSurfaceOutput(&output, screenshotFile, withoutImageData); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&surfaceID, "surface", "", "open surface ID")
	if !actionsOnly {
		command.Flags().StringVar(&screenshotOut, "screenshot-out", "", "securely create this file with the matching PNG screenshot")
		command.Flags().BoolVar(&withoutImageData, "without-image-data", false, "omit screenshot base64 from JSON output")
	}
	return command
}

func (a *application) surfaceActCommand(back bool) *cobra.Command {
	var accountID string
	var surfaceID string
	var actionID string
	var text string
	var confirmed bool
	var screenshotOut string
	var withoutImageData bool
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
			if !back {
				if err := requireValue("action", actionID); err != nil {
					return err
				}
			}
			screenshotFile, err := reserveOptionalPrivateFile(screenshotOut)
			if err != nil {
				return err
			}
			if screenshotFile != nil {
				defer screenshotFile.discard()
			}
			if back {
				snapshot, err := a.snapshotSurface(command.Context(), accountID, surfaceID, true)
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
			input := map[string]any{
				"account": accountID, "id": surfaceID, "action_id": actionID,
				"confirmed": confirmed, "without_image_data": withoutImageData && screenshotFile == nil,
			}
			if !back && command.Flags().Changed("text") {
				input["text"] = text
			}
			if err := client.Post(command.Context(), "/v1/surfaces/act", input, &output); err != nil {
				return err
			}
			if err := finalizeSurfaceOutput(&output, screenshotFile, withoutImageData); err != nil {
				return err
			}
			return a.write(output)
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&surfaceID, "surface", "", "open surface ID")
	if !back {
		command.Flags().StringVar(&actionID, "action", "", "semantic action ID from the latest snapshot")
		command.Flags().StringVar(&text, "text", "", "replacement text for an advertised input action; an empty value clears it")
	}
	command.Flags().BoolVar(&confirmed, "confirm", false, "confirm the exact selected medium/unknown-risk or external-write action")
	command.Flags().StringVar(&screenshotOut, "screenshot-out", "", "securely create this file with the matching PNG screenshot")
	command.Flags().BoolVar(&withoutImageData, "without-image-data", false, "omit screenshot base64 from JSON output")
	return command
}

func (a *application) surfaceExportCommand() *cobra.Command {
	var accountID, surfaceID, assetToken, outputPath string
	command := &cobra.Command{
		Use:   "export",
		Short: "Export one rendered image crop from the latest surface snapshot",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			for name, value := range map[string]string{
				"account": accountID, "surface": surfaceID, "asset-token": assetToken, "output": outputPath,
			} {
				if err := requireValue(name, value); err != nil {
					return err
				}
			}
			client, err := a.daemonClient()
			if err != nil {
				return err
			}
			var output surfaceExportOutput
			if err := client.Post(command.Context(), "/v1/surfaces/export", map[string]string{
				"account": accountID, "id": surfaceID, "asset_token": assetToken,
			}, &output); err != nil {
				return err
			}
			data, err := base64.StdEncoding.DecodeString(output.DataBase64)
			if err != nil || len(data) == 0 {
				return internalError("daemon returned invalid rendered image data", err)
			}
			digest := sha256.Sum256(data)
			if output.Asset.Fidelity != "rendered" || output.Asset.MediaType != "image/png" ||
				output.Asset.Bytes != int64(len(data)) || output.Asset.SHA256 != hex.EncodeToString(digest[:]) {
				return internalError("daemon returned inconsistent rendered image metadata", nil)
			}
			if err := writeNewPrivateFile(outputPath, data); err != nil {
				return err
			}
			return a.write(map[string]any{"asset": output.Asset, "output": outputPath})
		},
	}
	command.Flags().StringVar(&accountID, "account", "", "opaque account ID or unique alias")
	command.Flags().StringVar(&surfaceID, "surface", "", "open surface ID")
	command.Flags().StringVar(&assetToken, "asset-token", "", "short-lived token from the latest surface snapshot")
	command.Flags().StringVar(&outputPath, "output", "", "new local PNG path; existing files are never overwritten")
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

func (a *application) snapshotSurface(ctx context.Context, accountID, surfaceID string, withoutImageData bool) (surfaceOutput, error) {
	client, err := a.daemonClient()
	if err != nil {
		return surfaceOutput{}, err
	}
	var output surfaceOutput
	err = client.Post(ctx, "/v1/surfaces/snapshot", map[string]any{
		"account": accountID, "id": surfaceID, "without_image_data": withoutImageData,
	}, &output)
	return output, err
}

func verifiedScreenshot(output surfaceOutput) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(output.ScreenshotBase64)
	if err != nil || len(data) == 0 {
		return nil, internalError("daemon returned invalid screenshot data", err)
	}
	digest := sha256.Sum256(data)
	if output.Surface.ScreenshotSHA256 == "" || output.Surface.ScreenshotSHA256 != hex.EncodeToString(digest[:]) {
		return nil, internalError("daemon returned a screenshot that does not match its surface snapshot", nil)
	}
	return data, nil
}

func finalizeSurfaceOutput(output *surfaceOutput, screenshotFile *reservedPrivateFile, withoutImageData bool) error {
	if withoutImageData && screenshotFile == nil {
		output.ScreenshotBase64 = ""
		return nil
	}
	data, err := verifiedScreenshot(*output)
	if err != nil {
		return err
	}
	if screenshotFile != nil {
		if err := screenshotFile.writeAndCommit(data); err != nil {
			return err
		}
		output.ScreenshotPath = screenshotFile.path
		output.ScreenshotBase64 = ""
	}
	return nil
}

func reserveOptionalPrivateFile(path string) (*reservedPrivateFile, error) {
	if path == "" {
		return nil, nil
	}
	return reserveNewPrivateFileWithStat(path, unix.Fstat)
}

type privateFileStatFunc func(int, *unix.Stat_t) error

type reservedPrivateFile struct {
	path        string
	name        string
	directoryFD int
	file        *os.File
	created     unix.Stat_t
	fstat       privateFileStatFunc
	committed   bool
}

func reserveNewPrivateFileWithStat(path string, fstat privateFileStatFunc) (*reservedPrivateFile, error) {
	cleanPath := filepath.Clean(path)
	name := filepath.Base(cleanPath)
	if path == "" || name == "." || name == string(filepath.Separator) {
		return nil, api.NewError(400, api.CodeInvalidArgument, "output path must name a new file")
	}
	directoryFD, err := openPrivateOutputDirectory(filepath.Dir(cleanPath))
	if err != nil {
		return nil, api.WrapError(400, api.CodeInvalidArgument, "cannot securely open output directory", err)
	}

	fd, err := unix.Openat2(directoryFD, name, &unix.OpenHow{
		Flags:   uint64(unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC),
		Mode:    0o600,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		_ = unix.Close(directoryFD)
		return nil, api.WrapError(400, api.CodeInvalidArgument, "cannot securely create output file", err)
	}
	var created, inspected unix.Stat_t
	if err := unix.Fstat(fd, &created); err != nil {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(directoryFD, name, 0)
		_ = unix.Close(directoryFD)
		return nil, internalError("inspect output file", err)
	}
	if err := fstat(fd, &inspected); err != nil {
		_ = unix.Close(fd)
		unlinkCreatedOutput(directoryFD, name, &created)
		_ = unix.Close(directoryFD)
		return nil, internalError("inspect output file", err)
	}
	if inspected.Dev != created.Dev || inspected.Ino != created.Ino {
		_ = unix.Close(fd)
		unlinkCreatedOutput(directoryFD, name, &created)
		_ = unix.Close(directoryFD)
		return nil, internalError("output file identity changed during inspection", nil)
	}
	if err := validatePrivateOutputFile(&inspected); err != nil {
		_ = unix.Close(fd)
		unlinkCreatedOutput(directoryFD, name, &created)
		_ = unix.Close(directoryFD)
		return nil, api.WrapError(400, api.CodeInvalidArgument, "filesystem did not create a private output file", err)
	}
	file := os.NewFile(uintptr(fd), cleanPath)
	if file == nil {
		_ = unix.Close(fd)
		unlinkCreatedOutput(directoryFD, name, &created)
		_ = unix.Close(directoryFD)
		return nil, internalError("create output file handle", nil)
	}
	return &reservedPrivateFile{
		path: cleanPath, name: name, directoryFD: directoryFD,
		file: file, created: created, fstat: fstat,
	}, nil
}

func (output *reservedPrivateFile) writeAndCommit(data []byte) error {
	if output == nil || output.file == nil || output.committed {
		return internalError("output file reservation is unavailable", nil)
	}
	written, err := output.file.Write(data)
	if err != nil {
		return internalError("write output file", err)
	}
	if written != len(data) {
		return internalError("write output file", fmt.Errorf("short write: wrote %d of %d bytes", written, len(data)))
	}
	var final, inspected unix.Stat_t
	if err := unix.Fstat(int(output.file.Fd()), &final); err != nil {
		return internalError("reinspect output file", err)
	}
	if final.Dev != output.created.Dev || final.Ino != output.created.Ino {
		return internalError("output file identity changed while writing", nil)
	}
	if err := output.fstat(int(output.file.Fd()), &inspected); err != nil {
		return internalError("reinspect output file", err)
	}
	if inspected.Dev != final.Dev || inspected.Ino != final.Ino {
		return internalError("output file identity changed during final inspection", nil)
	}
	if err := validatePrivateOutputFile(&inspected); err != nil {
		return api.WrapError(400, api.CodeInvalidArgument, "output file stopped being private", err)
	}
	var entry unix.Stat_t
	if err := unix.Fstatat(output.directoryFD, output.name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return internalError("reinspect output path", err)
	}
	if entry.Dev != final.Dev || entry.Ino != final.Ino {
		return internalError("output path changed while writing", nil)
	}
	if err := output.file.Close(); err != nil {
		output.file = nil
		return internalError("close output file", err)
	}
	output.file = nil
	output.committed = true
	_ = unix.Close(output.directoryFD)
	output.directoryFD = -1
	return nil
}

func (output *reservedPrivateFile) discard() {
	if output == nil {
		return
	}
	if output.file != nil {
		_ = output.file.Close()
		output.file = nil
	}
	if !output.committed && output.directoryFD >= 0 {
		unlinkCreatedOutput(output.directoryFD, output.name, &output.created)
	}
	if output.directoryFD >= 0 {
		_ = unix.Close(output.directoryFD)
		output.directoryFD = -1
	}
}

func writeNewPrivateFile(path string, data []byte) error {
	return writeNewPrivateFileWithStat(path, data, unix.Fstat)
}

func writeNewPrivateFileWithStat(path string, data []byte, fstat privateFileStatFunc) error {
	output, err := reserveNewPrivateFileWithStat(path, fstat)
	if err != nil {
		return err
	}
	defer output.discard()
	return output.writeAndCommit(data)
}

func validatePrivateOutputFile(stat *unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("created output is not a regular file")
	}
	if stat.Mode&0o7777 != 0o600 {
		return fmt.Errorf("created output permissions are %#o, want 0600", stat.Mode&0o7777)
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("created output link count is %d, want 1", stat.Nlink)
	}
	return nil
}

func validatePrivateOutputDirectory(stat *unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("output parent is not a directory")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("output directory owner is uid %d, want %d", stat.Uid, os.Geteuid())
	}
	if stat.Mode&0o022 != 0 {
		return fmt.Errorf("output directory permissions %#o allow another uid to replace files", stat.Mode&0o7777)
	}
	return nil
}

func openPrivateOutputDirectory(path string) (int, error) {
	return openPrivateOutputDirectoryWithHook(path, nil)
}

func openPrivateOutputDirectoryWithHook(path string, afterOpen func(string)) (int, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return -1, fmt.Errorf("resolve output directory: %w", err)
	}
	absolutePath = filepath.Clean(absolutePath)
	currentFD, err := unix.Open(
		string(filepath.Separator), unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0,
	)
	if err != nil {
		return -1, fmt.Errorf("open filesystem root for output: %w", err)
	}
	var current unix.Stat_t
	if err := unix.Fstat(currentFD, &current); err != nil {
		_ = unix.Close(currentFD)
		return -1, fmt.Errorf("inspect filesystem root for output: %w", err)
	}
	currentPath := string(filepath.Separator)
	relative := strings.TrimPrefix(absolutePath, string(filepath.Separator))
	if relative != "" {
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			childFD, err := unix.Openat2(currentFD, component, &unix.OpenHow{
				Flags:   uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC),
				Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
			})
			if err != nil {
				_ = unix.Close(currentFD)
				return -1, fmt.Errorf("open output directory component %q: %w", component, err)
			}
			var child unix.Stat_t
			if err := unix.Fstat(childFD, &child); err != nil {
				_ = unix.Close(childFD)
				_ = unix.Close(currentFD)
				return -1, fmt.Errorf("inspect output directory component %q: %w", component, err)
			}
			childPath := filepath.Join(currentPath, component)
			if afterOpen != nil {
				afterOpen(childPath)
			}
			if err := validatePrivateOutputAncestor(&current, &child); err != nil {
				_ = unix.Close(childFD)
				_ = unix.Close(currentFD)
				return -1, fmt.Errorf("unsafe output ancestor %q: %w", currentPath, err)
			}
			_ = unix.Close(currentFD)
			currentFD = childFD
			current = child
			currentPath = childPath
		}
	}
	if err := validatePrivateOutputDirectory(&current); err != nil {
		_ = unix.Close(currentFD)
		return -1, err
	}
	return currentFD, nil
}

func validatePrivateOutputAncestor(parent, child *unix.Stat_t) error {
	if parent.Mode&unix.S_IFMT != unix.S_IFDIR || child.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("output ancestry contains a non-directory")
	}
	if parent.Uid != 0 && parent.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("ancestor owner uid %d is outside the trusted user boundary", parent.Uid)
	}
	if parent.Mode&0o022 != 0 && (parent.Mode&unix.S_ISVTX == 0 || child.Uid != uint32(os.Geteuid())) {
		return fmt.Errorf("ancestor permissions %#o allow another uid to replace the path", parent.Mode&0o7777)
	}
	return nil
}

func unlinkCreatedOutput(directoryFD int, name string, created *unix.Stat_t) {
	var current unix.Stat_t
	if err := unix.Fstatat(directoryFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return
	}
	if current.Dev != created.Dev || current.Ino != created.Ino {
		return
	}
	_ = unix.Unlinkat(directoryFD, name, 0)
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
