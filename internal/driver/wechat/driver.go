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
	"image/draw"
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
var errRenderedAssetTooLarge = errors.New("rendered asset exceeds safe size limit")

const (
	defaultVerificationTimeout = 15 * time.Second
	defaultMaxAttachmentBytes  = int64(512 << 20)
	defaultMaxMessageRunes     = 100_000
	surfaceAssetTokenTTL       = 2 * time.Minute
	maxSurfaceScreenshotBytes  = 32 << 20
	maxSurfaceImageDimension   = 16_384
	maxSurfaceImagePixels      = 64_000_000
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
	id               string
	accountID        string
	generation       string
	screenshotSHA256 string
	windowIdentity   string
	actions          map[string]BackendAction
	consumedActions  map[string]struct{}
	replayTombstones map[string]struct{}
	assets           map[string]shared.SurfaceAsset
	surface          shared.Surface
}

type sendMemo struct {
	digest string
	result shared.SendResult
	err    error
}

type boundedBytesBuffer struct {
	bytes.Buffer
	maximum int
}

func (b *boundedBytesBuffer) Write(contents []byte) (int, error) {
	remaining := b.maximum - b.Len()
	if remaining <= 0 {
		return 0, errRenderedAssetTooLarge
	}
	if len(contents) <= remaining {
		return b.Buffer.Write(contents)
	}
	written, _ := b.Buffer.Write(contents[:remaining])
	return written, errRenderedAssetTooLarge
}

var _ shared.Driver = (*Driver)(nil)
var _ shared.AccountPurger = (*Driver)(nil)
var _ shared.AuthActionDriver = (*Driver)(nil)
var _ shared.NamedSurfaceOpener = (*Driver)(nil)
var _ shared.SurfaceAssetExporter = (*Driver)(nil)

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
	screenshot := append([]byte(nil), probe.ScreenshotPNG...)
	if len(screenshot) == 0 {
		if hasImageBoundAuthAction(probe.Actions) {
			return shared.AuthSnapshot{}, fmt.Errorf("%w: image-bound authentication action has no matching screenshot", ErrClientIncompatible)
		}
		screenshot, err = d.backend.Screenshot(ctx)
		if err != nil {
			return shared.AuthSnapshot{}, err
		}
	}
	var qrCode []byte
	if probe.State == shared.StateAuthRequired && (probe.AuthKind == "" || probe.AuthKind == shared.AuthQR) {
		// Accessibility bounds identify a likely QR node, but they are not a
		// reliable pixel boundary. Keep the complete screenshot so scaled or
		// overflowing QR rendering is never clipped at the reported node edge.
		qrCode = screenshot
	}
	kind := probe.AuthKind
	if kind == "" {
		kind = shared.AuthQR
	}
	return shared.AuthSnapshot{
		Kind: kind, State: probe.State, Prompt: probe.Prompt,
		QRCodePNG: qrCode, ScreenshotPNG: screenshot,
		CanSubmitCode: probe.CanSubmitCode,
		Actions:       append([]shared.AuthAction(nil), probe.Actions...),
		ObservedAt:    probe.ObservedAt,
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

func (d *Driver) PerformAuthAction(ctx context.Context, request shared.AuthActionRequest) error {
	expectedGeneration, ok := savedAccountLoginGeneration(request.ActionID)
	if !ok {
		return fmt.Errorf("%w: authentication action is not advertised", ErrActionStale)
	}
	if !request.Confirmed {
		return fmt.Errorf("%w: authentication action requires explicit user confirmation", ErrUserActionRequired)
	}

	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if !d.started() {
		return ErrNotStarted
	}
	probe, err := d.backend.Probe(ctx)
	if err != nil {
		return err
	}
	if probe.State != shared.StateAuthRequired || !savedAccountLoginActionAdvertised(probe.Actions, request.ActionID) {
		return fmt.Errorf("%w: saved-account login confirmation is no longer advertised", ErrActionStale)
	}
	if err := d.backend.ContinueSavedAccountLogin(ctx, expectedGeneration); err != nil {
		if definitiveAuthActionRejection(err) || shared.AuthActionWasConsumed(err) {
			return err
		}
		// Losing the Docker/control response cannot prove that the official
		// client rejected the activation. Consume the one-time web action so a
		// retry cannot click the login control twice.
		return shared.MarkAuthActionConsumed(err)
	}
	return nil
}

func savedAccountLoginGeneration(actionID string) (string, bool) {
	if !strings.HasPrefix(actionID, savedAccountLoginActionPrefix) {
		return "", false
	}
	generation := strings.TrimPrefix(actionID, savedAccountLoginActionPrefix)
	return generation, authGenerationPattern.MatchString(generation)
}

func hasImageBoundAuthAction(actions []shared.AuthAction) bool {
	for _, action := range actions {
		if action.ImageBound {
			return true
		}
	}
	return false
}

func savedAccountLoginActionAdvertised(actions []shared.AuthAction, actionID string) bool {
	matches := 0
	for _, action := range actions {
		if action.ID != actionID {
			continue
		}
		if !action.RequiresConfirmation || !action.ImageBound {
			return false
		}
		matches++
	}
	return matches == 1
}

func definitiveAuthActionRejection(err error) bool {
	return errors.Is(err, ErrNotStarted) || errors.Is(err, ErrActionStale) ||
		errors.Is(err, ErrTargetAmbiguous) || errors.Is(err, ErrUserActionRequired) ||
		errors.Is(err, ErrClientIncompatible) || errors.Is(err, ErrAuthRequired) ||
		errors.Is(err, ErrInvalidArgument)
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
	if probe.State != shared.StateOnline {
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
	return d.storeSurface(id, backendSurface)
}

func (d *Driver) OpenNamedSurface(ctx context.Context, target shared.NamedSurface) (shared.Surface, error) {
	if !d.started() {
		return shared.Surface{}, ErrNotStarted
	}
	if target.Kind != "miniprogram" {
		return shared.Surface{}, fmt.Errorf("%w: only named mini programs are supported", ErrUnsupported)
	}
	name := strings.TrimSpace(target.Name)
	if name == "" || utf8.RuneCountInString(name) > 128 || strings.ContainsRune(name, '\x00') {
		return shared.Surface{}, errors.New("mini-program name must contain between 1 and 128 characters")
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	backendSurface, err := d.backend.OpenNamedSurface(ctx, target.Kind, name)
	if err != nil {
		return shared.Surface{}, err
	}
	if backendSurface.Generation == "" || backendSurface.ScreenshotSHA256 == "" || backendSurface.WindowIdentity == "" {
		return shared.Surface{}, fmt.Errorf("%w: named mini-program open did not return a frame-bound snapshot", ErrClientIncompatible)
	}
	id, err := randomOpaqueID("wxsurf_")
	if err != nil {
		return shared.Surface{}, err
	}
	d.mu.Lock()
	d.surfaces = make(map[string]*surfaceSession)
	d.mu.Unlock()
	return d.storeSurface(id, backendSurface)
}

func (d *Driver) SnapshotSurface(ctx context.Context, id string) (shared.Surface, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	session := d.surfaces[id]
	d.mu.Unlock()
	if session == nil {
		return shared.Surface{}, ErrSurfaceMissing
	}
	surface, err := d.backend.SnapshotSurface(ctx, session.windowIdentity)
	if err != nil {
		return shared.Surface{}, err
	}
	if surface.WindowIdentity != session.windowIdentity {
		return shared.Surface{}, ErrActionStale
	}
	return d.storeSurface(id, surface)
}

func (d *Driver) ActSurface(ctx context.Context, id string, request shared.SurfaceAction) (shared.Surface, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	session := d.surfaces[id]
	var action BackendAction
	actionFound := false
	if session != nil {
		action, actionFound = session.actions[request.ActionID]
	}
	d.mu.Unlock()
	if !actionFound {
		return shared.Surface{}, ErrActionStale
	}
	risk := strings.ToLower(strings.TrimSpace(action.Action.Risk))
	effect := strings.ToLower(strings.TrimSpace(action.Action.Effect))
	if action.Action.Disabled || risk == "high" || risk == "sensitive" || risk == "destructive" ||
		effect == "high_risk" || effect == "sensitive" || effect == "destructive" {
		return shared.Surface{}, ErrUserActionRequired
	}
	if !request.TextProvided && request.Text != "" {
		return shared.Surface{}, fmt.Errorf("%w: surface action text presence is inconsistent", ErrInvalidArgument)
	}
	if action.Action.Kind == "input" {
		if !request.TextProvided {
			return shared.Surface{}, fmt.Errorf("%w: text is required by a surface input action; pass an explicit empty string to clear it", ErrInvalidArgument)
		}
	} else if request.TextProvided {
		return shared.Surface{}, fmt.Errorf("%w: text is only accepted by a surface input action", ErrInvalidArgument)
	}
	if request.TextProvided && (utf8.RuneCountInString(request.Text) > 4_096 || strings.ContainsRune(request.Text, '\x00')) {
		return shared.Surface{}, fmt.Errorf("%w: surface input text must not exceed 4096 characters or contain NUL", ErrInvalidArgument)
	}
	if (risk != "low" || effect == "external_write") && !request.Confirmed {
		return shared.Surface{}, ErrConfirmationRequired
	}
	d.mu.Lock()
	// A semantic action is a one-shot capability. Consume it before the
	// backend dispatch so retries after a timeout cannot double-click.
	delete(session.actions, request.ActionID)
	session.consumedActions[request.ActionID] = struct{}{}
	d.mu.Unlock()
	surface, err := d.backend.ActSurface(ctx, session.windowIdentity, action.Locator, request.Text)
	if shouldTombstoneReplay(effect, err == nil) {
		d.mu.Lock()
		current := d.surfaces[id]
		if current != nil && current.windowIdentity == session.windowIdentity {
			current.replayTombstones[action.ReplayID] = struct{}{}
		}
		d.mu.Unlock()
	}
	if err != nil {
		return shared.Surface{}, err
	}
	return d.storeSurface(id, surface)
}

func shouldTombstoneReplay(effect string, succeeded bool) bool {
	switch effect {
	case "observe", "search_input":
		return false
	case "navigate":
		return !succeeded
	default:
		return true
	}
}

func (d *Driver) CloseSurface(ctx context.Context, id string) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	session := d.surfaces[id]
	if session != nil {
		// Closing is a one-shot action. Invalidate the session before dispatch
		// so a timeout cannot turn a caller retry into a second back/close.
		delete(d.surfaces, id)
	}
	d.mu.Unlock()
	if session == nil {
		return ErrSurfaceMissing
	}
	if err := d.backend.CloseSurface(ctx, session.windowIdentity); err != nil {
		return err
	}
	return nil
}

func (d *Driver) storeSurface(id string, backend BackendSurface) (shared.Surface, error) {
	if authenticationSurfaceKind(backend.Kind) {
		return shared.Surface{}, ErrAuthRequired
	}
	if backend.WindowIdentity == "" {
		return shared.Surface{}, fmt.Errorf("%w: surface response has no bound window identity", ErrClientIncompatible)
	}
	if len(backend.Screenshot) == 0 || len(backend.Screenshot) > maxSurfaceScreenshotBytes {
		return shared.Surface{}, fmt.Errorf("%w: surface screenshot size is outside the safe limit", ErrClientIncompatible)
	}
	configuration, err := png.DecodeConfig(bytes.NewReader(backend.Screenshot))
	if err != nil || configuration.Width <= 0 || configuration.Height <= 0 ||
		configuration.Width > maxSurfaceImageDimension || configuration.Height > maxSurfaceImageDimension ||
		int64(configuration.Width)*int64(configuration.Height) > maxSurfaceImagePixels {
		return shared.Surface{}, fmt.Errorf("%w: surface screenshot is not a bounded PNG", ErrClientIncompatible)
	}
	digest := sha256.Sum256(backend.Screenshot)
	screenshotSHA256 := hex.EncodeToString(digest[:])
	if backend.ScreenshotSHA256 != "" && !strings.EqualFold(backend.ScreenshotSHA256, screenshotSHA256) {
		return shared.Surface{}, fmt.Errorf("%w: surface screenshot digest changed across the driver boundary", ErrClientIncompatible)
	}
	if (len(backend.Elements) != 0 || len(backend.Assets) != 0 || backend.Viewport != nil) && backend.Generation == "" {
		return shared.Surface{}, fmt.Errorf("%w: semantic surface metadata has no generation", ErrClientIncompatible)
	}
	now := d.config.Now().UTC()
	surface := shared.Surface{
		ID: id, Kind: backend.Kind, Title: backend.Title, URL: backend.URL,
		AppID: backend.AppID, Generation: backend.Generation,
		Screenshot: append([]byte(nil), backend.Screenshot...), ScreenshotSHA256: screenshotSHA256,
		OCRText: backend.SemanticText, Viewport: backend.Viewport, ObservedAt: now,
	}
	d.mu.Lock()
	previous := d.surfaces[id]
	consumedActions := make(map[string]struct{})
	replayTombstones := make(map[string]struct{})
	if previous != nil {
		for actionID := range previous.consumedActions {
			consumedActions[actionID] = struct{}{}
		}
		for replayID := range previous.replayTombstones {
			replayTombstones[replayID] = struct{}{}
		}
	}
	d.mu.Unlock()
	actions := make(map[string]BackendAction, len(backend.Actions))
	for _, action := range backend.Actions {
		if action.Action.ID == "" || action.ReplayID == "" || action.Locator == "" {
			continue
		}
		if _, consumed := consumedActions[action.Action.ID]; consumed {
			continue
		}
		if _, tombstoned := replayTombstones[action.ReplayID]; tombstoned {
			continue
		}
		actions[action.Action.ID] = action
		surface.Actions = append(surface.Actions, action.Action)
	}
	for _, element := range backend.Elements {
		candidateIDs := element.ActionIDs
		if len(candidateIDs) == 0 && element.ActionID != "" {
			candidateIDs = []string{element.ActionID}
		}
		element.ActionID = ""
		element.ActionIDs = nil
		seen := make(map[string]struct{}, len(candidateIDs))
		for _, actionID := range candidateIDs {
			if _, duplicate := seen[actionID]; duplicate {
				continue
			}
			seen[actionID] = struct{}{}
			if _, active := actions[actionID]; active {
				element.ActionIDs = append(element.ActionIDs, actionID)
			}
		}
		if len(element.ActionIDs) == 1 {
			element.ActionID = element.ActionIDs[0]
		}
		surface.Elements = append(surface.Elements, element)
	}
	assets := make(map[string]shared.SurfaceAsset, len(backend.Assets))
	assetIDs := make(map[string]bool, len(backend.Assets))
	for _, asset := range backend.Assets {
		if asset.ID == "" || asset.Token == "" || asset.Kind == "" || asset.Bounds.Width <= 0 || asset.Bounds.Height <= 0 || assetIDs[asset.ID] {
			continue
		}
		token, err := randomOpaqueID("wxasset_")
		if err != nil {
			return shared.Surface{}, err
		}
		assetIDs[asset.ID] = true
		asset.Token = token
		asset.ExpiresAt = now.Add(surfaceAssetTokenTTL)
		assets[token] = asset
		surface.Assets = append(surface.Assets, asset)
	}
	d.mu.Lock()
	accountID := ""
	if d.account != nil {
		accountID = d.account.AccountID
	}
	d.surfaces[id] = &surfaceSession{
		id: id, accountID: accountID, generation: backend.Generation,
		screenshotSHA256: screenshotSHA256, windowIdentity: backend.WindowIdentity,
		actions: actions, consumedActions: consumedActions, replayTombstones: replayTombstones,
		assets: assets, surface: surface,
	}
	d.mu.Unlock()
	return surface, nil
}

func authenticationSurfaceKind(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "auth", "auth_required", "authentication", "login", "qr", "sms":
		return true
	default:
		return false
	}
}

func (d *Driver) ExportSurfaceAsset(ctx context.Context, id, token string) (shared.SurfaceAssetExport, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return shared.SurfaceAssetExport{}, err
	}
	d.mu.Lock()
	session := d.surfaces[id]
	var asset shared.SurfaceAsset
	found := false
	accountID := ""
	if d.account != nil {
		accountID = d.account.AccountID
	}
	if session != nil && session.accountID == accountID {
		asset, found = session.assets[token]
	}
	if found && !d.config.Now().UTC().Before(asset.ExpiresAt) {
		delete(session.assets, token)
		found = false
	}
	var screenshot []byte
	var generation, screenshotSHA256 string
	if found {
		screenshot = append([]byte(nil), session.surface.Screenshot...)
		generation = session.generation
		screenshotSHA256 = session.screenshotSHA256
	}
	d.mu.Unlock()
	if session == nil {
		return shared.SurfaceAssetExport{}, ErrSurfaceMissing
	}
	if !found || token == "" || generation == "" || screenshotSHA256 == "" {
		return shared.SurfaceAssetExport{}, ErrAssetStale
	}
	digest := sha256.Sum256(screenshot)
	if hex.EncodeToString(digest[:]) != screenshotSHA256 {
		return shared.SurfaceAssetExport{}, fmt.Errorf("%w: cached surface screenshot no longer matches its generation", ErrClientIncompatible)
	}
	if len(screenshot) > maxSurfaceScreenshotBytes {
		return shared.SurfaceAssetExport{}, fmt.Errorf("%w: cached surface screenshot is too large", ErrClientIncompatible)
	}
	configuration, err := png.DecodeConfig(bytes.NewReader(screenshot))
	if err != nil {
		return shared.SurfaceAssetExport{}, fmt.Errorf("%w: inspect surface screenshot: %v", ErrClientIncompatible, err)
	}
	if configuration.Width <= 0 || configuration.Height <= 0 ||
		configuration.Width > maxSurfaceImageDimension || configuration.Height > maxSurfaceImageDimension ||
		int64(configuration.Width)*int64(configuration.Height) > maxSurfaceImagePixels {
		return shared.SurfaceAssetExport{}, fmt.Errorf("%w: surface screenshot dimensions are outside the safe export limit", ErrClientIncompatible)
	}
	decoded, err := png.Decode(bytes.NewReader(screenshot))
	if err != nil {
		return shared.SurfaceAssetExport{}, fmt.Errorf("%w: decode surface screenshot: %v", ErrClientIncompatible, err)
	}
	imageBounds := decoded.Bounds()
	if asset.Bounds.X < imageBounds.Min.X || asset.Bounds.Y < imageBounds.Min.Y ||
		asset.Bounds.Width <= 0 || asset.Bounds.Height <= 0 ||
		asset.Bounds.Width > imageBounds.Max.X-asset.Bounds.X ||
		asset.Bounds.Height > imageBounds.Max.Y-asset.Bounds.Y {
		return shared.SurfaceAssetExport{}, fmt.Errorf("%w: asset bounds are outside the matching screenshot", ErrClientIncompatible)
	}
	rectangle := image.Rect(asset.Bounds.X, asset.Bounds.Y, asset.Bounds.X+asset.Bounds.Width, asset.Bounds.Y+asset.Bounds.Height)
	crop := image.NewRGBA(image.Rect(0, 0, rectangle.Dx(), rectangle.Dy()))
	draw.Draw(crop, crop.Bounds(), decoded, rectangle.Min, draw.Src)
	encoded := boundedBytesBuffer{maximum: maxSurfaceScreenshotBytes}
	if err := png.Encode(&encoded, crop); err != nil {
		if errors.Is(err, errRenderedAssetTooLarge) {
			return shared.SurfaceAssetExport{}, fmt.Errorf("%w: rendered asset exceeds the safe export limit", ErrClientIncompatible)
		}
		return shared.SurfaceAssetExport{}, fmt.Errorf("encode rendered asset: %w", err)
	}
	data := encoded.Bytes()
	assetDigest := sha256.Sum256(data)
	return shared.SurfaceAssetExport{
		SurfaceID: id, AssetID: asset.ID, Fidelity: "rendered", MediaType: "image/png",
		SHA256: hex.EncodeToString(assetDigest[:]), Bytes: int64(len(data)), Data: data,
	}, nil
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
		shared.CapabilityAuthQR:                shared.SupportBeta,
		shared.CapabilityAuthSMS:               shared.SupportBeta,
		shared.CapabilityMiniProgramOpenByName: shared.SupportExperimental,
		shared.CapabilitySurfaceAct:            shared.SupportExperimental,
		shared.CapabilitySurfaceAssetExport:    shared.SupportExperimental,
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
