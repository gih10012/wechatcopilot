// Package wechat implements the personal WeChat driver by automating the
// official Linux client in an isolated, persistent X11 container.
package wechat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
)

var authCodePattern = regexp.MustCompile(`^[0-9]{4,10}$`)

const (
	defaultVerificationTimeout = 15 * time.Second
	defaultMaxAttachmentBytes  = int64(512 << 20)
	defaultMaxMessageRunes     = 100_000
)

type IndexFactory func(Profile) (MessageIndex, error)

type Config struct {
	Docker              DockerConfig
	Backend             Backend
	Index               MessageIndex
	IndexFactory        IndexFactory
	Profiles            ProfileManager
	VerificationTimeout time.Duration
	MaxAttachmentBytes  int64
	Now                 func() time.Time
}

type Driver struct {
	config  Config
	backend Backend

	mu          sync.Mutex
	operationMu sync.Mutex
	account     *shared.AccountRuntime
	profile     *Profile
	index       MessageIndex
	surfaces    map[string]*surfaceSession
	sendMemos   map[string]sendMemo
}

type surfaceSession struct {
	id      string
	actions map[string]BackendAction
	surface shared.Surface
}

type sendMemo struct {
	digest string
	result shared.SendResult
	err    error
}

var _ shared.Driver = (*Driver)(nil)
var _ shared.AccountPurger = (*Driver)(nil)

func New(config Config) (*Driver, error) {
	backend := config.Backend
	if backend == nil {
		var err error
		backend, err = NewDockerBackend(config.Docker)
		if err != nil {
			return nil, err
		}
	}
	if config.VerificationTimeout <= 0 {
		config.VerificationTimeout = defaultVerificationTimeout
	}
	if config.MaxAttachmentBytes <= 0 {
		config.MaxAttachmentBytes = defaultMaxAttachmentBytes
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Driver{
		config: config, backend: backend, index: UnavailableIndex{},
		surfaces: make(map[string]*surfaceSession), sendMemos: make(map[string]sendMemo),
	}, nil
}

// NewFactory produces isolated Driver instances for the daemon's driver
// registry. A Config with a concrete Backend is intended only for tests; real
// factories should supply Docker configuration.
func NewFactory(config Config) shared.Factory {
	return func(shared.AccountRuntime) (shared.Driver, error) {
		if config.Backend != nil || config.Index != nil {
			return nil, errors.New("multi-account factory requires per-instance Docker backend and IndexFactory")
		}
		return New(config)
	}
}

func (*Driver) Platform() shared.Platform { return shared.PlatformWeChat }

func (d *Driver) Start(ctx context.Context, account shared.AccountRuntime) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	if d.account != nil {
		d.mu.Unlock()
		return ErrAlreadyStarted
	}
	d.mu.Unlock()

	profile, err := d.config.Profiles.Ensure(account)
	if err != nil {
		return err
	}
	index := d.config.Index
	if d.config.IndexFactory != nil {
		index, err = d.config.IndexFactory(profile)
		if err != nil {
			return fmt.Errorf("initialize WeChat message index: %w", err)
		}
	}
	if err := d.backend.Start(ctx, profile); err != nil {
		return err
	}
	if index == nil {
		index = NewUIIndex(d.backend, account.AccountID, d.config.Now)
	}

	d.mu.Lock()
	accountCopy := account
	d.account = &accountCopy
	d.profile = &profile
	d.index = index
	d.surfaces = make(map[string]*surfaceSession)
	d.sendMemos = make(map[string]sendMemo)
	d.mu.Unlock()
	return nil
}

func (d *Driver) Stop(ctx context.Context) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if err := d.backend.Stop(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	d.account = nil
	d.profile = nil
	d.index = UnavailableIndex{}
	d.surfaces = make(map[string]*surfaceSession)
	d.sendMemos = make(map[string]sendMemo)
	d.mu.Unlock()
	return nil
}

// Purge removes only the stopped, ownership-labelled container for account.
// Persistent account paths remain owned by the account store's purge flow.
func (d *Driver) Purge(ctx context.Context, account shared.AccountRuntime) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	started := d.account != nil
	d.mu.Unlock()
	if started {
		return errors.New("cannot purge an active WeChat account")
	}
	purger, ok := d.backend.(interface {
		Purge(context.Context, shared.AccountRuntime) error
	})
	if !ok {
		return errors.New("WeChat backend does not support account purge")
	}
	return purger.Purge(ctx, account)
}

func (d *Driver) Status(ctx context.Context) (shared.Status, error) {
	if !d.started() {
		return shared.Status{
			State: shared.StateStopped, Capabilities: d.capabilities(ctx),
			ObservedAt: d.config.Now().UTC(),
		}, nil
	}
	probe, err := d.backend.Probe(ctx)
	if err != nil {
		return shared.Status{}, err
	}
	return shared.Status{
		State: probe.State, Identity: probe.Identity, Reason: probe.Reason,
		ClientVersion: probe.ClientVersion, Capabilities: d.capabilities(ctx),
		ObservedAt: probe.ObservedAt,
	}, nil
}

func (d *Driver) AuthSnapshot(ctx context.Context) (shared.AuthSnapshot, error) {
	if !d.started() {
		return shared.AuthSnapshot{}, ErrNotStarted
	}
	probe, err := d.backend.Probe(ctx)
	if err != nil {
		return shared.AuthSnapshot{}, err
	}
	screenshot, err := d.backend.Screenshot(ctx)
	if err != nil {
		return shared.AuthSnapshot{}, err
	}
	var qrCode []byte
	if probe.State == shared.StateAuthRequired && (probe.AuthKind == "" || probe.AuthKind == shared.AuthQR) {
		qrCode = screenshot
	}
	if len(qrCode) > 0 && probe.QRBounds != nil {
		if cropped, cropErr := cropPNG(screenshot, *probe.QRBounds); cropErr == nil {
			qrCode = cropped
		}
	}
	kind := probe.AuthKind
	if kind == "" {
		kind = shared.AuthQR
	}
	return shared.AuthSnapshot{
		Kind: kind, State: probe.State, Prompt: probe.Prompt,
		QRCodePNG: qrCode, ScreenshotPNG: screenshot,
		CanSubmitCode: probe.CanSubmitCode, ObservedAt: probe.ObservedAt,
	}, nil
}

func (d *Driver) SubmitAuthCode(ctx context.Context, code string) error {
	if !d.started() {
		return ErrNotStarted
	}
	code = strings.TrimSpace(code)
	if !authCodePattern.MatchString(code) {
		return errors.New("authentication code must contain 4 to 10 digits")
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	return d.backend.SubmitAuthCode(ctx, code)
}

func (d *Driver) ListConversations(ctx context.Context, query shared.ConversationQuery) ([]shared.Conversation, error) {
	index := d.currentIndex()
	if query.Limit < 0 || query.Limit > 1000 {
		return nil, errors.New("conversation limit must be between 0 and 1000")
	}
	return index.ListConversations(ctx, query)
}

func (d *Driver) ReadMessages(ctx context.Context, query shared.MessageQuery) ([]shared.Message, error) {
	index := d.currentIndex()
	if query.Limit < 0 || query.Limit > 5000 {
		return nil, errors.New("message limit must be between 0 and 5000")
	}
	return index.ReadMessages(ctx, query)
}

func (d *Driver) Send(ctx context.Context, request shared.SendRequest) (shared.SendResult, error) {
	if !d.started() {
		return shared.SendResult{}, ErrNotStarted
	}
	if strings.TrimSpace(request.ConversationID) == "" {
		return shared.SendResult{}, errors.New("conversation id is required")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return shared.SendResult{}, errors.New("idempotency key is required")
	}
	if utf8.RuneCountInString(request.Text) > defaultMaxMessageRunes {
		return shared.SendResult{}, errors.New("message text is too large")
	}
	if request.Text == "" && len(request.Attachments) == 0 && request.ShareSurfaceID == "" {
		return shared.SendResult{}, errors.New("send request is empty")
	}
	if len(request.Attachments) > 20 {
		return shared.SendResult{}, errors.New("a send request can contain at most 20 attachments")
	}

	// The official client has one foreground UI. Serializing all mutating UI
	// operations also closes the idempotency race between concurrent callers.
	d.operationMu.Lock()
	defer d.operationMu.Unlock()

	digest := sendDigest(request)
	d.mu.Lock()
	if memo, ok := d.sendMemos[request.IdempotencyKey]; ok {
		d.mu.Unlock()
		if memo.digest != digest {
			return shared.SendResult{}, errors.New("idempotency key was already used for different content")
		}
		return memo.result, memo.err
	}
	d.mu.Unlock()

	probe, err := d.backend.Probe(ctx)
	if err != nil {
		return shared.SendResult{}, err
	}
	if probe.State == shared.StateAuthRequired {
		return shared.SendResult{}, ErrAuthRequired
	}
	if probe.State != shared.StateOnline && probe.State != shared.StateDegraded {
		return shared.SendResult{}, fmt.Errorf("wechat client is not online: %s", probe.State)
	}

	index := d.currentIndex()
	conversation, err := index.Conversation(ctx, request.ConversationID)
	if err != nil {
		return shared.SendResult{}, err
	}
	if conversation.ID != request.ConversationID || strings.TrimSpace(conversation.Title) == "" {
		return shared.SendResult{}, ErrConversationMissing
	}
	staged, cleanup, err := d.stageAttachments(request.Attachments)
	if err != nil {
		return shared.SendResult{}, err
	}
	defer cleanup()

	shareLocator := ""
	if request.ShareSurfaceID != "" {
		d.mu.Lock()
		session := d.surfaces[request.ShareSurfaceID]
		shareCandidates := 0
		blockedShare := false
		if session != nil {
			for _, action := range session.actions {
				if action.Action.Kind != "share" {
					continue
				}
				if action.Action.Disabled || action.Action.Risk == "high" {
					blockedShare = true
					continue
				}
				shareCandidates++
				shareLocator = action.Locator
			}
		}
		d.mu.Unlock()
		if shareCandidates > 1 {
			return shared.SendResult{}, ErrTargetAmbiguous
		}
		if shareCandidates == 0 && blockedShare {
			return shared.SendResult{}, ErrUserActionRequired
		}
		if shareCandidates == 0 {
			return shared.SendResult{}, ErrActionStale
		}
	}

	startedAt := d.config.Now().UTC().Add(-2 * time.Second)
	match := OutgoingMatch{
		ConversationID: conversation.ID, Text: request.Text,
		AttachmentCount: len(request.Attachments), NotBefore: startedAt,
		Timeout: d.config.VerificationTimeout,
	}
	if preparer, ok := index.(OutgoingBaselinePreparer); ok {
		if err := preparer.PrepareOutgoing(ctx, match); err != nil {
			return shared.SendResult{}, fmt.Errorf("prepare outgoing UI verification: %w", err)
		}
	}
	err = d.backend.Send(ctx, UISendRequest{
		ConversationID: conversation.ID, Title: conversation.Title,
		Locator: conversation.ExternalID, Text: request.Text,
		Attachments: staged, ShareLocator: shareLocator,
	})
	if err != nil {
		result := shared.SendResult{
			Uncertain: true,
			Detail:    "the UI operation started but its completion could not be determined",
		}
		resultErr := fmt.Errorf("%w: %v", ErrSendUncertain, err)
		d.mu.Lock()
		d.sendMemos[request.IdempotencyKey] = sendMemo{digest: digest, result: result, err: resultErr}
		d.mu.Unlock()
		return result, resultErr
	}

	message, verifyErr := index.WaitOutgoing(ctx, match)
	result := shared.SendResult{}
	resultErr := error(nil)
	if verifyErr != nil {
		result.Uncertain = true
		result.Detail = "official client accepted the UI action, but the read-only index did not confirm it"
		resultErr = ErrSendUncertain
	} else {
		result.MessageID = message.ID
		result.Verified = true
	}
	d.mu.Lock()
	d.sendMemos[request.IdempotencyKey] = sendMemo{digest: digest, result: result, err: resultErr}
	d.mu.Unlock()
	return result, resultErr
}

func (d *Driver) OpenSurface(ctx context.Context, reference string) (shared.Surface, error) {
	if !d.started() {
		return shared.Surface{}, ErrNotStarted
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	target, err := d.currentIndex().ResolveSurface(ctx, reference)
	if err != nil {
		return shared.Surface{}, err
	}
	backendSurface, err := d.backend.OpenSurface(ctx, target)
	if err != nil {
		return shared.Surface{}, err
	}
	id, err := randomOpaqueID("wxsurf_")
	if err != nil {
		return shared.Surface{}, err
	}
	d.mu.Lock()
	d.surfaces = make(map[string]*surfaceSession)
	d.mu.Unlock()
	return d.storeSurface(id, backendSurface), nil
}

func (d *Driver) SnapshotSurface(ctx context.Context, id string) (shared.Surface, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if !d.hasSurface(id) {
		return shared.Surface{}, ErrSurfaceMissing
	}
	surface, err := d.backend.SnapshotSurface(ctx)
	if err != nil {
		return shared.Surface{}, err
	}
	return d.storeSurface(id, surface), nil
}

func (d *Driver) ActSurface(ctx context.Context, id string, request shared.SurfaceAction) (shared.Surface, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	session := d.surfaces[id]
	var action BackendAction
	found := false
	if session != nil {
		action, found = session.actions[request.ActionID]
	}
	d.mu.Unlock()
	if !found {
		return shared.Surface{}, ErrActionStale
	}
	if action.Action.Disabled || action.Action.Risk == "high" {
		return shared.Surface{}, ErrUserActionRequired
	}
	if request.Text != "" && action.Action.Kind != "input" {
		return shared.Surface{}, errors.New("text is only accepted by a surface input action")
	}
	surface, err := d.backend.ActSurface(ctx, action.Locator, request.Text)
	if err != nil {
		return shared.Surface{}, err
	}
	return d.storeSurface(id, surface), nil
}

func (d *Driver) CloseSurface(ctx context.Context, id string) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if !d.hasSurface(id) {
		return ErrSurfaceMissing
	}
	if err := d.backend.CloseSurface(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.surfaces, id)
	d.mu.Unlock()
	return nil
}

func (d *Driver) storeSurface(id string, backend BackendSurface) shared.Surface {
	surface := shared.Surface{
		ID: id, Kind: backend.Kind, Title: backend.Title, URL: backend.URL,
		AppID: backend.AppID, Screenshot: backend.Screenshot,
		OCRText: backend.SemanticText, ObservedAt: d.config.Now().UTC(),
	}
	actions := make(map[string]BackendAction, len(backend.Actions))
	for _, action := range backend.Actions {
		if action.Action.ID == "" || action.Locator == "" {
			continue
		}
		actions[action.Action.ID] = action
		surface.Actions = append(surface.Actions, action.Action)
	}
	d.mu.Lock()
	d.surfaces[id] = &surfaceSession{id: id, actions: actions, surface: surface}
	d.mu.Unlock()
	return surface
}

func (d *Driver) hasSurface(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.surfaces[id]
	return ok
}

func (d *Driver) started() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.account != nil
}

func (d *Driver) currentIndex() MessageIndex {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.index
}

func (d *Driver) capabilities(ctx context.Context) map[string]shared.Support {
	result := shared.CapabilityMap(map[string]shared.Support{
		shared.CapabilityAuthQR:  shared.SupportBeta,
		shared.CapabilityAuthSMS: shared.SupportBeta,
	})
	index := d.currentIndex()
	if index != nil && index.Available(ctx) {
		result[shared.CapabilityMessagesHistory] = shared.SupportBeta
		result[shared.CapabilityMessagesWatch] = shared.SupportBeta
		result[shared.CapabilityOfficialAccountsRead] = shared.SupportBeta
		result[shared.CapabilityMessagesSend] = shared.SupportBeta
		result[shared.CapabilityAttachmentsSend] = shared.SupportExperimental
		result[shared.CapabilityWebOpen] = shared.SupportExperimental
		result[shared.CapabilityMiniProgramOpen] = shared.SupportExperimental
		result[shared.CapabilitySurfaceAct] = shared.SupportExperimental
		if _, uiFallback := index.(*UIIndex); uiFallback {
			result[shared.CapabilityMessagesVisible] = shared.SupportBeta
			result[shared.CapabilityMessagesHistory] = shared.SupportUnsupported
			result[shared.CapabilityMessagesWatch] = shared.SupportExperimental
			result[shared.CapabilityOfficialAccountsRead] = shared.SupportUnsupported
		}
	}
	return result
}

func (d *Driver) stageAttachments(attachments []shared.Attachment) ([]string, func(), error) {
	if len(attachments) == 0 {
		return nil, func() {}, nil
	}
	d.mu.Lock()
	profile := d.profile
	d.mu.Unlock()
	if profile == nil {
		return nil, func() {}, ErrNotStarted
	}
	outbox := filepath.Join(profile.Runtime, "outbox")
	if err := ensurePrivateDirectory(outbox); err != nil {
		return nil, func() {}, err
	}
	var total int64
	var staged []string
	cleanup := func() {
		for _, path := range staged {
			_ = os.Remove(path)
		}
	}
	for _, attachment := range attachments {
		path, err := cleanAbsolute(attachment.LocalPath)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			cleanup()
			return nil, func() {}, fmt.Errorf("attachment %q must be a regular, non-symlink file", path)
		}
		total += info.Size()
		if total > d.config.MaxAttachmentBytes {
			cleanup()
			return nil, func() {}, fmt.Errorf("attachments exceed %d bytes", d.config.MaxAttachmentBytes)
		}
		id, err := randomOpaqueID("")
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		name := sanitizeFilename(attachment.Name)
		if name == "" {
			name = sanitizeFilename(filepath.Base(path))
		}
		destination := filepath.Join(outbox, id+"-"+name)
		if err := copyRegularFile(path, destination, info); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		staged = append(staged, destination)
	}
	containerPaths := make([]string, 0, len(staged))
	for _, path := range staged {
		containerPaths = append(containerPaths, "/wechatcopilot/runtime/outbox/"+filepath.Base(path))
	}
	return containerPaths, cleanup, nil
}

func copyRegularFile(source, destination string, expected fs.FileInfo) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	openedInfo, err := in.Stat()
	if err != nil {
		return err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(expected, openedInfo) {
		return errors.New("attachment changed before it could be staged")
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	written, err := io.Copy(out, io.LimitReader(in, expected.Size()+1))
	if err != nil {
		return err
	}
	if written != expected.Size() {
		return errors.New("attachment changed while it was being staged")
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func sanitizeFilename(value string) string {
	value = filepath.Base(strings.TrimSpace(value))
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, value)
	value = strings.Trim(value, ". ")
	if len(value) > 180 {
		value = value[:180]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	if value == "" {
		return "attachment"
	}
	return value
}

func cropPNG(data []byte, rectangle Rectangle) ([]byte, error) {
	imageValue, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if rectangle.Width <= 0 || rectangle.Height <= 0 {
		return nil, errors.New("invalid QR bounds")
	}
	bounds := image.Rect(rectangle.X, rectangle.Y, rectangle.X+rectangle.Width, rectangle.Y+rectangle.Height).Intersect(imageValue.Bounds())
	if bounds.Empty() {
		return nil, errors.New("QR bounds are outside screenshot")
	}
	cropped := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			cropped.Set(x, y, imageValue.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, cropped); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func randomOpaqueID(prefix string) (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}

func sendDigest(request shared.SendRequest) string {
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, request.ConversationID)
	_, _ = io.WriteString(hasher, "\x00"+request.Text+"\x00"+request.ShareSurfaceID)
	for _, attachment := range request.Attachments {
		_, _ = io.WriteString(hasher, "\x00"+attachment.Kind+"\x00"+attachment.Name+"\x00"+attachment.LocalPath)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
