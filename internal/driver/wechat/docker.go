package wechat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
	"golang.org/x/sys/unix"
)

var imageReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,511}$`)
var sha256DigestPattern = regexp.MustCompile(`^[A-Fa-f0-9]{64}$`)
var dockerImageIDPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var dockerByteSizePattern = regexp.MustCompile(`^([1-9][0-9]*)([bBkKmMgG])$`)

type DockerConfig struct {
	Binary                 string
	Image                  string
	AppImagePath           string
	ExpectedAppImageSHA256 string
	Display                string
	UID                    int
	GID                    int
	Memory                 string
	SHMSize                string
	Runner                 CommandRunner
}

type DockerBackend struct {
	config       DockerConfig
	memoryBytes  int64
	shmSizeBytes int64
	mu           sync.Mutex
	profile      *Profile
	container    string
}

type controlRequest struct {
	Operation           string   `json:"operation"`
	ConversationID      string   `json:"conversation_id,omitempty"`
	ConversationTitle   string   `json:"conversation_title,omitempty"`
	ConversationLocator string   `json:"conversation_locator,omitempty"`
	Text                string   `json:"text,omitempty"`
	Attachments         []string `json:"attachments,omitempty"`
	Reference           string   `json:"reference,omitempty"`
	AccessibleLabel     string   `json:"accessible_label,omitempty"`
	Locator             string   `json:"locator,omitempty"`
	ShareLocator        string   `json:"share_locator,omitempty"`
}

type controlResponse struct {
	OK                  bool                  `json:"ok"`
	Error               string                `json:"error,omitempty"`
	Code                string                `json:"code,omitempty"`
	State               string                `json:"state,omitempty"`
	Reason              string                `json:"reason,omitempty"`
	ClientVersion       string                `json:"client_version,omitempty"`
	AuthKind            string                `json:"auth_kind,omitempty"`
	Prompt              string                `json:"prompt,omitempty"`
	CanSubmitCode       bool                  `json:"can_submit_code,omitempty"`
	Identity            *controlIdentity      `json:"identity,omitempty"`
	QRBounds            *Rectangle            `json:"qr_bounds,omitempty"`
	Surface             *controlSurface       `json:"surface,omitempty"`
	Conversations       []controlConversation `json:"conversations,omitempty"`
	Messages            []controlMessage      `json:"messages,omitempty"`
	ConversationTitle   string                `json:"conversation_title,omitempty"`
	ConversationLocator string                `json:"conversation_locator,omitempty"`
}

type controlConversation struct {
	Title     string `json:"title"`
	Kind      string `json:"kind,omitempty"`
	Unread    int    `json:"unread,omitempty"`
	Locator   string `json:"locator"`
	Ambiguous bool   `json:"ambiguous,omitempty"`
}

type controlMessage struct {
	Text            string  `json:"text,omitempty"`
	Kind            string  `json:"kind,omitempty"`
	SenderName      string  `json:"sender_name,omitempty"`
	Outgoing        bool    `json:"outgoing,omitempty"`
	AccessibleLabel string  `json:"accessible_label,omitempty"`
	SurfaceKind     string  `json:"surface_kind,omitempty"`
	Confidence      float64 `json:"confidence,omitempty"`
}

type controlIdentity struct {
	PlatformID  string `json:"platform_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type controlSurface struct {
	Kind         string          `json:"kind,omitempty"`
	Title        string          `json:"title,omitempty"`
	URL          string          `json:"url,omitempty"`
	AppID        string          `json:"app_id,omitempty"`
	SemanticText string          `json:"semantic_text,omitempty"`
	Actions      []controlAction `json:"actions,omitempty"`
}

type controlAction struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Risk     string `json:"risk,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	Locator  string `json:"locator"`
}

func NewDockerBackend(config DockerConfig) (*DockerBackend, error) {
	if config.Binary == "" {
		config.Binary = "docker"
	}
	if config.Image == "" || !imageReferencePattern.MatchString(config.Image) {
		return nil, errors.New("a valid local WeChat runtime image is required")
	}
	if config.Display == "" {
		config.Display = ":99"
	}
	if config.UID <= 0 {
		config.UID = os.Getuid()
	}
	if config.GID <= 0 {
		config.GID = os.Getgid()
	}
	if config.Memory == "" {
		config.Memory = "4g"
	}
	if config.SHMSize == "" {
		config.SHMSize = "1g"
	}
	memoryBytes, err := parseDockerByteSize(config.Memory)
	if err != nil {
		return nil, fmt.Errorf("invalid WeChat container memory limit: %w", err)
	}
	shmSizeBytes, err := parseDockerByteSize(config.SHMSize)
	if err != nil {
		return nil, fmt.Errorf("invalid WeChat container shared-memory size: %w", err)
	}
	if config.Runner == nil {
		config.Runner = ExecCommandRunner{}
	}
	expectedDigest, err := normalizeExpectedAppImageSHA256(config.ExpectedAppImageSHA256)
	if err != nil {
		return nil, err
	}
	config.ExpectedAppImageSHA256 = expectedDigest
	appImage, err := normalizeAppImagePath(config.AppImagePath)
	if err != nil {
		return nil, err
	}
	config.AppImagePath = appImage
	return &DockerBackend{config: config, memoryBytes: memoryBytes, shmSizeBytes: shmSizeBytes}, nil
}

func parseDockerByteSize(value string) (int64, error) {
	matches := dockerByteSizePattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(matches) != 3 {
		return 0, errors.New("size must be a positive integer followed by b, k, m, or g")
	}
	number, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return 0, errors.New("size is outside the supported range")
	}
	multiplier := int64(1)
	switch strings.ToLower(matches[2]) {
	case "k":
		multiplier = 1 << 10
	case "m":
		multiplier = 1 << 20
	case "g":
		multiplier = 1 << 30
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if number > maxInt64/multiplier {
		return 0, errors.New("size is outside the supported range")
	}
	return number * multiplier, nil
}

func normalizeAppImagePath(path string) (string, error) {
	if path == "" {
		return "", errors.New("official WeChat AppImage path is required")
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", errors.New("official WeChat AppImage path cannot be resolved")
	}
	// Resolve parent links but preserve the last component so a configured
	// symlink cannot be normalized into an apparently regular artifact.
	parent, err := cleanAbsolute(filepath.Dir(absolute))
	if err != nil {
		return "", errors.New("official WeChat AppImage path cannot be resolved")
	}
	resolved := filepath.Join(parent, filepath.Base(absolute))
	if err := (ProfileManager{}).rejectProtected(resolved); err != nil {
		return "", errors.New("official WeChat AppImage path overlaps a protected client profile")
	}
	return resolved, nil
}

func normalizeExpectedAppImageSHA256(value string) (string, error) {
	if !sha256DigestPattern.MatchString(value) {
		return "", errors.New("expected official WeChat AppImage SHA-256 must contain exactly 64 hexadecimal characters")
	}
	return strings.ToLower(value), nil
}

func verifyAppImage(path, expectedDigest string) (string, string, error) {
	expectedDigest, err := normalizeExpectedAppImageSHA256(expectedDigest)
	if err != nil {
		return "", "", err
	}
	resolved, err := normalizeAppImagePath(path)
	if err != nil {
		return "", "", err
	}
	pathInfo, err := os.Lstat(resolved)
	if err != nil {
		return "", "", appImageAccessError(err)
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("official WeChat AppImage must be a regular, non-symlink file")
	}
	if pathInfo.Mode().Perm()&0o111 == 0 {
		return "", "", errors.New("official WeChat AppImage is not executable")
	}

	fd, err := unix.Open(resolved, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return "", "", errors.New("official WeChat AppImage must be a regular, non-symlink file")
		}
		return "", "", appImageAccessError(err)
	}
	file := os.NewFile(uintptr(fd), "verified-wechat-appimage")
	if file == nil {
		_ = unix.Close(fd)
		return "", "", errors.New("official WeChat AppImage could not be opened safely")
	}
	defer file.Close()

	openedInfo, err := file.Stat()
	if err != nil {
		return "", "", errors.New("official WeChat AppImage could not be inspected safely")
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return "", "", errors.New("official WeChat AppImage changed while it was being verified")
	}
	if openedInfo.Mode().Perm()&0o111 == 0 {
		return "", "", errors.New("official WeChat AppImage is not executable")
	}

	hasher := sha256.New()
	var magic [4]byte
	if _, err := io.ReadFull(io.TeeReader(file, hasher), magic[:]); err != nil {
		return "", "", errors.New("official WeChat AppImage is not a valid ELF executable")
	}
	if magic != [4]byte{0x7f, 'E', 'L', 'F'} {
		return "", "", errors.New("official WeChat AppImage does not have an ELF header")
	}
	if _, err := io.Copy(hasher, file); err != nil {
		return "", "", errors.New("official WeChat AppImage could not be read safely")
	}
	afterInfo, err := file.Stat()
	if err != nil {
		return "", "", errors.New("official WeChat AppImage could not be inspected safely")
	}
	if !os.SameFile(openedInfo, afterInfo) || openedInfo.Size() != afterInfo.Size() || !openedInfo.ModTime().Equal(afterInfo.ModTime()) {
		return "", "", errors.New("official WeChat AppImage changed while it was being verified")
	}
	actualDigest := hex.EncodeToString(hasher.Sum(nil))
	if actualDigest != expectedDigest {
		return "", "", errors.New("official WeChat AppImage SHA-256 does not match the configured digest")
	}
	return resolved, actualDigest, nil
}

func appImageAccessError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("official WeChat AppImage is unavailable")
	case errors.Is(err, os.ErrPermission):
		return errors.New("official WeChat AppImage cannot be accessed with the current permissions")
	default:
		return errors.New("official WeChat AppImage could not be opened safely")
	}
}

func (b *DockerBackend) Start(ctx context.Context, profile Profile) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.profile != nil {
		return ErrAlreadyStarted
	}
	appImage, clientFingerprint, err := verifyAppImage(b.config.AppImagePath, b.config.ExpectedAppImageSHA256)
	if err != nil {
		return err
	}
	b.config.AppImagePath = appImage
	runtimeImage, err := b.inspectRuntimeImage(ctx)
	if err != nil {
		return err
	}
	container := containerName(profile.AccountID)

	existing, err := b.inspectContainer(ctx, container)
	if err != nil {
		return err
	}
	if existing.Exists {
		if err := b.verifyContainer(existing, runtimeImage, profile, clientFingerprint); err != nil {
			return fmt.Errorf("%w (container %q)", err, container)
		}
		if !existing.State.Running {
			if _, _, err := verifyAppImage(b.config.AppImagePath, b.config.ExpectedAppImageSHA256); err != nil {
				return err
			}
			if _, err := b.run(ctx, "start", container); err != nil {
				return err
			}
		}
		b.profile = &profile
		b.container = container
		return nil
	}
	if _, _, err := verifyAppImage(b.config.AppImagePath, b.config.ExpectedAppImageSHA256); err != nil {
		return err
	}

	args := []string{
		"run", "--detach", "--pull=never", "--name", container,
		"--hostname", profile.Hostname,
		"--label", "io.wechatcopilot.driver=wechat-linux",
		"--label", "io.wechatcopilot.account=" + profile.AccountID,
		"--label", "io.wechatcopilot.profile=" + fingerprint(profile.Root),
		"--label", "io.wechatcopilot.client=" + clientFingerprint,
		"--label", "io.wechatcopilot.runtime=" + fingerprint(runtimeImage.ID),
		"--label", "io.wechatcopilot.config=" + b.containerConfigFingerprint(profile, runtimeImage.ID),
		"--user", strconv.Itoa(b.config.UID) + ":" + strconv.Itoa(b.config.GID),
		"--security-opt", "no-new-privileges=true",
		"--cap-drop", "ALL",
		"--network", "bridge",
		"--pids-limit", "2048",
		"--memory", b.config.Memory,
		"--memory-swap", b.config.Memory,
		"--shm-size", b.config.SHMSize,
		"--tmpfs", "/tmp:rw,nosuid,nodev,exec,mode=1777",
		"--volume", b.config.AppImagePath + ":/opt/wechat/WeChat.AppImage:ro",
		"--volume", profile.ClientHome + ":/home/wechat:rw",
		"--volume", profile.Files + ":/home/wechat/WeChat_Files:rw",
		"--volume", profile.Runtime + ":/wechatcopilot/runtime:rw",
		"--volume", filepath.Join(profile.Root, "machine-id") + ":/etc/machine-id:ro",
		"--env", "WECHAT_DISPLAY=" + b.config.Display,
		"--env", "XDG_RUNTIME_DIR=/wechatcopilot/runtime/xdg",
		runtimeImage.ID,
	}
	if _, err := b.run(ctx, args...); err != nil {
		return err
	}
	b.profile = &profile
	b.container = container
	return nil
}

func (b *DockerBackend) Purge(ctx context.Context, account shared.AccountRuntime) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.profile != nil {
		return errors.New("cannot purge a running WeChat driver")
	}
	if !accountIDPattern.MatchString(account.AccountID) || !filepath.IsAbs(account.StateDir) {
		return errors.New("valid account id and absolute state directory are required for purge")
	}
	stateRoot, err := cleanAbsolute(account.StateDir)
	if err != nil {
		return err
	}
	if err := (ProfileManager{}).rejectProtected(stateRoot); err != nil {
		return err
	}
	name := containerName(account.AccountID)
	inspection, err := b.inspectContainer(ctx, name)
	if err != nil {
		return err
	}
	if !inspection.Exists {
		return nil
	}
	if inspection.Name != "/"+name || inspection.label("io.wechatcopilot.driver") != "wechat-linux" ||
		inspection.label("io.wechatcopilot.account") != account.AccountID ||
		inspection.label("io.wechatcopilot.profile") != fingerprint(stateRoot) {
		return fmt.Errorf("refusing to purge container %q because its ownership labels do not match", name)
	}
	if inspection.State.Running {
		return fmt.Errorf("refusing to purge running container %q", name)
	}
	_, err = b.run(ctx, "container", "rm", name)
	return err
}

func (b *DockerBackend) Stop(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.profile == nil {
		return nil
	}
	_, err := b.run(ctx, "stop", "--time", "20", b.container)
	if err == nil {
		b.profile = nil
		b.container = ""
	}
	return err
}

func (b *DockerBackend) Probe(ctx context.Context) (ProbeResult, error) {
	response, err := b.control(ctx, controlRequest{Operation: "probe"})
	if err != nil {
		return ProbeResult{}, err
	}
	state := parseRuntimeState(response.State)
	result := ProbeResult{
		State:         state,
		Reason:        response.Reason,
		ClientVersion: response.ClientVersion,
		AuthKind:      shared.AuthKind(response.AuthKind),
		Prompt:        response.Prompt,
		CanSubmitCode: response.CanSubmitCode,
		ObservedAt:    time.Now().UTC(),
		QRBounds:      response.QRBounds,
	}
	if response.Identity != nil {
		result.Identity = &shared.Identity{
			PlatformID:  response.Identity.PlatformID,
			DisplayName: response.Identity.DisplayName,
		}
	}
	return result, nil
}

func (b *DockerBackend) Screenshot(ctx context.Context) ([]byte, error) {
	container, err := b.activeContainer()
	if err != nil {
		return nil, err
	}
	data, err := b.run(ctx, "exec", "-i", container, "/opt/wechatcopilot/screenshot")
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, errors.New("wechat runtime returned an invalid screenshot")
	}
	return data, nil
}

func (b *DockerBackend) SubmitAuthCode(ctx context.Context, code string) error {
	_, err := b.control(ctx, controlRequest{Operation: "submit_auth_code", Text: code})
	return err
}

func (b *DockerBackend) ListVisibleConversations(ctx context.Context) ([]VisibleConversation, error) {
	response, err := b.control(ctx, controlRequest{Operation: "list_conversations"})
	if err != nil {
		return nil, err
	}
	result := make([]VisibleConversation, 0, len(response.Conversations))
	for _, item := range response.Conversations {
		result = append(result, VisibleConversation{
			Title: item.Title, Kind: item.Kind, Unread: item.Unread,
			Locator: item.Locator, Ambiguous: item.Ambiguous,
		})
	}
	return result, nil
}

func (b *DockerBackend) ReadVisibleMessages(ctx context.Context, title, locator string) (VisibleMessages, error) {
	response, err := b.control(ctx, controlRequest{
		Operation: "read_visible_messages", ConversationTitle: title,
		ConversationLocator: locator,
	})
	if err != nil {
		return VisibleMessages{}, err
	}
	result := VisibleMessages{
		ConversationTitle:   response.ConversationTitle,
		ConversationLocator: response.ConversationLocator,
		Messages:            make([]VisibleMessage, 0, len(response.Messages)),
	}
	for _, item := range response.Messages {
		result.Messages = append(result.Messages, VisibleMessage{
			Text: item.Text, Kind: item.Kind, SenderName: item.SenderName,
			Outgoing: item.Outgoing, AccessibleLabel: item.AccessibleLabel,
			SurfaceKind: item.SurfaceKind, Confidence: item.Confidence,
		})
	}
	return result, nil
}

func (b *DockerBackend) Send(ctx context.Context, request UISendRequest) error {
	_, err := b.control(ctx, controlRequest{
		Operation:           "send",
		ConversationID:      request.ConversationID,
		ConversationTitle:   request.Title,
		ConversationLocator: request.Locator,
		Text:                request.Text,
		Attachments:         request.Attachments,
		ShareLocator:        request.ShareLocator,
	})
	return err
}

func (b *DockerBackend) OpenSurface(ctx context.Context, target SurfaceTarget) (BackendSurface, error) {
	response, err := b.control(ctx, controlRequest{
		Operation:           "open_surface",
		ConversationID:      target.ConversationID,
		ConversationTitle:   target.ConversationTitle,
		ConversationLocator: target.ConversationLocator,
		Reference:           target.Reference,
		AccessibleLabel:     target.AccessibleLabel,
	})
	if err != nil {
		return BackendSurface{}, err
	}
	return b.surfaceWithScreenshot(ctx, response.Surface)
}

func (b *DockerBackend) SnapshotSurface(ctx context.Context) (BackendSurface, error) {
	response, err := b.control(ctx, controlRequest{Operation: "snapshot_surface"})
	if err != nil {
		return BackendSurface{}, err
	}
	return b.surfaceWithScreenshot(ctx, response.Surface)
}

func (b *DockerBackend) ActSurface(ctx context.Context, locator, text string) (BackendSurface, error) {
	response, err := b.control(ctx, controlRequest{Operation: "act_surface", Locator: locator, Text: text})
	if err != nil {
		return BackendSurface{}, err
	}
	return b.surfaceWithScreenshot(ctx, response.Surface)
}

func (b *DockerBackend) CloseSurface(ctx context.Context) error {
	_, err := b.control(ctx, controlRequest{Operation: "close_surface"})
	return err
}

func (b *DockerBackend) surfaceWithScreenshot(ctx context.Context, surface *controlSurface) (BackendSurface, error) {
	if surface == nil {
		return BackendSurface{}, ErrSurfaceMissing
	}
	screenshot, err := b.Screenshot(ctx)
	if err != nil {
		return BackendSurface{}, err
	}
	result := BackendSurface{
		Kind: surface.Kind, Title: surface.Title, URL: surface.URL,
		AppID: surface.AppID, SemanticText: surface.SemanticText, Screenshot: screenshot,
	}
	for _, action := range surface.Actions {
		result.Actions = append(result.Actions, BackendAction{
			Action:  shared.Action{ID: action.ID, Label: action.Label, Kind: action.Kind, Risk: action.Risk, Disabled: action.Disabled},
			Locator: action.Locator,
		})
	}
	return result, nil
}

func (b *DockerBackend) control(ctx context.Context, request controlRequest) (controlResponse, error) {
	container, err := b.activeContainer()
	if err != nil {
		return controlResponse{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return controlResponse{}, err
	}
	data, err := b.config.Runner.Run(ctx, Command{
		Name:  b.config.Binary,
		Args:  []string{"exec", "-i", container, "/opt/wechatcopilot/control"},
		Stdin: payload,
	})
	if err != nil {
		return controlResponse{}, err
	}
	var response controlResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return controlResponse{}, fmt.Errorf("decode WeChat control response: %w", err)
	}
	if !response.OK {
		switch response.Code {
		case "AUTH_REQUIRED":
			return response, ErrAuthRequired
		case "TARGET_AMBIGUOUS":
			return response, ErrTargetAmbiguous
		case "SURFACE_MISSING":
			return response, ErrSurfaceMissing
		case "ACTION_STALE":
			return response, ErrActionStale
		case "CLIENT_INCOMPATIBLE":
			return response, ErrClientIncompatible
		default:
			return response, fmt.Errorf("wechat UI operation failed: %s", response.Error)
		}
	}
	return response, nil
}

func (b *DockerBackend) activeContainer() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.profile == nil || b.container == "" {
		return "", ErrNotStarted
	}
	return b.container, nil
}

type runtimeImageInspection struct {
	ID     string `json:"Id"`
	Config struct {
		Entrypoint   []string                   `json:"Entrypoint"`
		Cmd          []string                   `json:"Cmd"`
		WorkingDir   string                     `json:"WorkingDir"`
		Env          []string                   `json:"Env"`
		Labels       map[string]string          `json:"Labels"`
		ExposedPorts map[string]json.RawMessage `json:"ExposedPorts"`
		Volumes      map[string]json.RawMessage `json:"Volumes"`
	} `json:"Config"`
}

type containerMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
	Propagation string `json:"Propagation"`
}

type containerInspection struct {
	Exists bool   `json:"-"`
	Name   string `json:"Name"`
	Image  string `json:"Image"`
	Config struct {
		Image        string                     `json:"Image"`
		Hostname     string                     `json:"Hostname"`
		User         string                     `json:"User"`
		WorkingDir   string                     `json:"WorkingDir"`
		Entrypoint   []string                   `json:"Entrypoint"`
		Cmd          []string                   `json:"Cmd"`
		Env          []string                   `json:"Env"`
		Labels       map[string]string          `json:"Labels"`
		ExposedPorts map[string]json.RawMessage `json:"ExposedPorts"`
		Volumes      map[string]json.RawMessage `json:"Volumes"`
		AttachStdin  bool                       `json:"AttachStdin"`
		AttachStdout bool                       `json:"AttachStdout"`
		AttachStderr bool                       `json:"AttachStderr"`
		OpenStdin    bool                       `json:"OpenStdin"`
		StdinOnce    bool                       `json:"StdinOnce"`
		Tty          bool                       `json:"Tty"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	HostConfig struct {
		Privileged      bool                         `json:"Privileged"`
		NetworkMode     string                       `json:"NetworkMode"`
		PublishAllPorts bool                         `json:"PublishAllPorts"`
		PortBindings    map[string][]json.RawMessage `json:"PortBindings"`
		CapAdd          []string                     `json:"CapAdd"`
		CapDrop         []string                     `json:"CapDrop"`
		SecurityOpt     []string                     `json:"SecurityOpt"`
		PidsLimit       int64                        `json:"PidsLimit"`
		Memory          int64                        `json:"Memory"`
		MemorySwap      int64                        `json:"MemorySwap"`
		ShmSize         int64                        `json:"ShmSize"`
		Tmpfs           map[string]string            `json:"Tmpfs"`
		AutoRemove      bool                         `json:"AutoRemove"`
		PidMode         string                       `json:"PidMode"`
		IpcMode         string                       `json:"IpcMode"`
		UTSMode         string                       `json:"UTSMode"`
		UsernsMode      string                       `json:"UsernsMode"`
		CgroupnsMode    string                       `json:"CgroupnsMode"`
		Devices         []json.RawMessage            `json:"Devices"`
		DeviceRequests  []json.RawMessage            `json:"DeviceRequests"`
		RestartPolicy   struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Ports    map[string][]json.RawMessage `json:"Ports"`
		Networks map[string]json.RawMessage   `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []containerMount `json:"Mounts"`
}

func (b *DockerBackend) inspectRuntimeImage(ctx context.Context) (runtimeImageInspection, error) {
	data, err := b.run(ctx, "image", "inspect", b.config.Image)
	if err != nil {
		return runtimeImageInspection{}, fmt.Errorf("%w: configured WeChat runtime image is not available locally", ErrClientIncompatible)
	}
	var inspections []runtimeImageInspection
	if err := json.Unmarshal(data, &inspections); err != nil || len(inspections) != 1 {
		return runtimeImageInspection{}, fmt.Errorf("%w: cannot decode a unique WeChat runtime image inspection", ErrClientIncompatible)
	}
	inspection := inspections[0]
	inspection.ID = strings.ToLower(strings.TrimSpace(inspection.ID))
	if !dockerImageIDPattern.MatchString(inspection.ID) {
		return runtimeImageInspection{}, fmt.Errorf("%w: WeChat runtime image did not resolve to an immutable image ID", ErrClientIncompatible)
	}
	if !equalStrings(inspection.Config.Entrypoint, []string{"/opt/wechatcopilot/entrypoint"}) ||
		inspection.Config.WorkingDir != "/home/wechat" || len(inspection.Config.ExposedPorts) != 0 || len(inspection.Config.Volumes) != 0 {
		return runtimeImageInspection{}, fmt.Errorf("%w: configured image is not the expected port-free WeChat runtime", ErrClientIncompatible)
	}
	return inspection, nil
}

func (b *DockerBackend) inspectContainer(ctx context.Context, name string) (containerInspection, error) {
	data, err := b.run(ctx, "container", "inspect", name)
	if err != nil {
		// Docker inspect uses a non-zero status for a missing container. A
		// separate name-only listing distinguishes that expected case without
		// relying on localized stderr text.
		listed, listErr := b.run(ctx, "container", "ls", "--all", "--quiet", "--filter", "name=^/"+name+"$")
		if listErr != nil {
			return containerInspection{}, err
		}
		if strings.TrimSpace(string(listed)) == "" {
			return containerInspection{}, nil
		}
		return containerInspection{}, err
	}
	var inspections []containerInspection
	if err := json.Unmarshal(data, &inspections); err != nil || len(inspections) != 1 {
		return containerInspection{}, errors.New("cannot decode a unique WeChat container inspection")
	}
	inspection := inspections[0]
	inspection.Exists = true
	return inspection, nil
}

func (b *DockerBackend) verifyContainer(inspection containerInspection, image runtimeImageInspection, profile Profile, clientFingerprint string) error {
	expectedName := "/" + containerName(profile.AccountID)
	expectedUser := strconv.Itoa(b.config.UID) + ":" + strconv.Itoa(b.config.GID)
	expectedLabels := cloneStrings(image.Config.Labels)
	expectedLabels["io.wechatcopilot.driver"] = "wechat-linux"
	expectedLabels["io.wechatcopilot.account"] = profile.AccountID
	expectedLabels["io.wechatcopilot.profile"] = fingerprint(profile.Root)
	expectedLabels["io.wechatcopilot.client"] = clientFingerprint
	expectedLabels["io.wechatcopilot.runtime"] = fingerprint(image.ID)
	expectedLabels["io.wechatcopilot.config"] = b.containerConfigFingerprint(profile, image.ID)

	if inspection.Name != expectedName || inspection.Image != image.ID || inspection.Config.Image != image.ID ||
		inspection.Config.Hostname != profile.Hostname || inspection.Config.User != expectedUser ||
		inspection.Config.WorkingDir != image.Config.WorkingDir ||
		!equalStrings(inspection.Config.Entrypoint, image.Config.Entrypoint) ||
		!equalStrings(inspection.Config.Cmd, image.Config.Cmd) || !equalStringMaps(inspection.Config.Labels, expectedLabels) {
		return incompatibleContainer("identity, image, user, or immutable configuration")
	}
	if !containerEnvironmentMatches(inspection.Config.Env, image.Config.Env, map[string]string{
		"WECHAT_DISPLAY":  b.config.Display,
		"XDG_RUNTIME_DIR": "/wechatcopilot/runtime/xdg",
	}) {
		return incompatibleContainer("environment")
	}
	if len(inspection.Config.ExposedPorts) != 0 || len(inspection.Config.Volumes) != 0 ||
		inspection.Config.AttachStdin || inspection.Config.OpenStdin || inspection.Config.StdinOnce || inspection.Config.Tty {
		return incompatibleContainer("stdio or image-declared ports and volumes")
	}
	if inspection.HostConfig.Privileged || inspection.HostConfig.PublishAllPorts ||
		hasPublishedPorts(inspection.HostConfig.PortBindings) || hasPublishedPorts(inspection.NetworkSettings.Ports) ||
		len(inspection.HostConfig.CapAdd) != 0 || !oneCaseInsensitive(inspection.HostConfig.CapDrop, "ALL") ||
		!validNoNewPrivileges(inspection.HostConfig.SecurityOpt) || inspection.HostConfig.PidsLimit != 2048 ||
		inspection.HostConfig.Memory != b.memoryBytes || inspection.HostConfig.MemorySwap != b.memoryBytes ||
		inspection.HostConfig.ShmSize != b.shmSizeBytes ||
		inspection.HostConfig.AutoRemove || len(inspection.HostConfig.Devices) != 0 || len(inspection.HostConfig.DeviceRequests) != 0 ||
		inspection.HostConfig.PidMode != "" || !privateOrDefault(inspection.HostConfig.IpcMode) ||
		inspection.HostConfig.UTSMode != "" || inspection.HostConfig.UsernsMode != "" ||
		!privateOrDefault(inspection.HostConfig.CgroupnsMode) ||
		(inspection.HostConfig.RestartPolicy.Name != "" && inspection.HostConfig.RestartPolicy.Name != "no") {
		return incompatibleContainer("privileges, namespaces, capabilities, restart policy, or published ports")
	}
	if (inspection.HostConfig.NetworkMode != "bridge" && inspection.HostConfig.NetworkMode != "default") ||
		len(inspection.NetworkSettings.Networks) != 1 {
		return incompatibleContainer("network")
	}
	if _, ok := inspection.NetworkSettings.Networks["bridge"]; !ok {
		return incompatibleContainer("network")
	}
	if !validRuntimeTmpfs(inspection.HostConfig.Tmpfs) {
		return incompatibleContainer("temporary filesystem")
	}
	if !b.validContainerMounts(inspection.Mounts, profile) {
		return incompatibleContainer("bind mounts")
	}
	return nil
}

func (b *DockerBackend) containerConfigFingerprint(profile Profile, imageID string) string {
	values := []string{
		"wechat-container-v2", imageID, b.config.ExpectedAppImageSHA256,
		strconv.Itoa(b.config.UID), strconv.Itoa(b.config.GID), b.config.Display,
		b.config.Memory, b.config.SHMSize, profile.AccountID, profile.Root,
		profile.ClientHome, profile.Files, profile.Runtime, profile.Hostname,
	}
	return fingerprint(strings.Join(values, "\x00"))
}

func (b *DockerBackend) validContainerMounts(mounts []containerMount, profile Profile) bool {
	type expectedMount struct {
		source string
		rw     bool
	}
	expected := map[string]expectedMount{
		"/opt/wechat/WeChat.AppImage": {source: b.config.AppImagePath, rw: false},
		"/home/wechat":                {source: profile.ClientHome, rw: true},
		"/home/wechat/WeChat_Files":   {source: profile.Files, rw: true},
		"/wechatcopilot/runtime":      {source: profile.Runtime, rw: true},
		"/etc/machine-id":             {source: filepath.Join(profile.Root, "machine-id"), rw: false},
	}
	if len(mounts) != len(expected) {
		return false
	}
	seen := make(map[string]bool, len(mounts))
	for _, mount := range mounts {
		want, ok := expected[mount.Destination]
		if !ok || seen[mount.Destination] || mount.Type != "bind" || mount.RW != want.rw || mount.Propagation != "rprivate" ||
			!filepath.IsAbs(mount.Source) || filepath.Clean(mount.Source) != filepath.Clean(want.source) {
			return false
		}
		seen[mount.Destination] = true
	}
	return true
}

func (inspection containerInspection) label(name string) string {
	return inspection.Config.Labels[name]
}

func incompatibleContainer(detail string) error {
	return fmt.Errorf("%w: existing WeChat container does not match the required %s", ErrClientIncompatible, detail)
}

func cloneStrings(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+6)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalStringMaps(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func environmentMap(values []string) (map[string]string, bool) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 || parts[0] == "" {
			return nil, false
		}
		if _, duplicate := result[parts[0]]; duplicate {
			return nil, false
		}
		result[parts[0]] = parts[1]
	}
	return result, true
}

func containerEnvironmentMatches(actualValues, imageValues []string, overrides map[string]string) bool {
	actual, ok := environmentMap(actualValues)
	if !ok {
		return false
	}
	expected, ok := environmentMap(imageValues)
	if !ok {
		return false
	}
	for key, value := range overrides {
		expected[key] = value
	}
	return equalStringMaps(actual, expected)
}

func hasPublishedPorts(ports map[string][]json.RawMessage) bool {
	for _, bindings := range ports {
		if len(bindings) != 0 {
			return true
		}
	}
	return false
}

func oneCaseInsensitive(values []string, expected string) bool {
	return len(values) == 1 && strings.EqualFold(values[0], expected)
}

func validNoNewPrivileges(values []string) bool {
	return len(values) == 1 && (values[0] == "no-new-privileges" || values[0] == "no-new-privileges=true")
}

func privateOrDefault(value string) bool {
	return value == "" || value == "private"
}

func validRuntimeTmpfs(tmpfs map[string]string) bool {
	if len(tmpfs) != 1 {
		return false
	}
	value, ok := tmpfs["/tmp"]
	if !ok {
		return false
	}
	options := make(map[string]bool)
	for _, option := range strings.Split(value, ",") {
		options[option] = true
	}
	return len(options) == 5 && options["rw"] && options["nosuid"] && options["nodev"] && options["exec"] && options["mode=1777"]
}

func (b *DockerBackend) run(ctx context.Context, args ...string) ([]byte, error) {
	return b.config.Runner.Run(ctx, Command{Name: b.config.Binary, Args: args})
}

func containerName(accountID string) string {
	return "wechatcopilot-wechat-" + fingerprint(accountID)[:16]
}

func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func parseRuntimeState(value string) shared.RuntimeState {
	switch shared.RuntimeState(value) {
	case shared.StateStarting, shared.StateAuthRequired, shared.StateOnline, shared.StateDegraded, shared.StateOffline, shared.StateStopped:
		return shared.RuntimeState(value)
	default:
		return shared.StateDegraded
	}
}
