package wecom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	core "github.com/gih10012/wechatcopilot/internal/driver"
)

var (
	ErrInvalidArgument       = core.NewFailure(core.FailureInvalidArgument, "WeCom operation arguments are invalid")
	ErrAuthRequired          = core.NewFailure(core.FailureAuthRequired, "WeCom authentication is required")
	ErrTargetAmbiguous       = core.NewFailure(core.FailureTargetAmbiguous, "WeCom conversation target is ambiguous")
	ErrUnsupportedCapability = core.NewFailure(core.FailureUnsupported, "WeCom driver capability is unsupported")
	ErrSendUncertain         = core.NewFailure(core.FailureSendUncertain, "WeCom send result is uncertain")
	ErrConfirmationRequired  = core.NewFailure(core.FailureConfirmationRequired, "WeCom surface action requires explicit confirmation")
	ErrUserActionRequired    = core.NewFailure(core.FailureUserActionRequired, "direct user interaction is required")
	ErrNotFound              = core.NewFailure(core.FailureNotFound, "requested WeCom object was not found")
	ErrStale                 = core.NewFailure(core.FailureStale, "WeCom UI state is stale")
	ErrClientIncompatible    = core.NewFailure(core.FailureClientIncompatible, "the configured WeCom client runtime is incompatible")
)

const (
	weComLoginWxAuthActivity      = "com.tencent.wework.login.controller.LoginWxAuthActivity"
	weComSMSVerifyActivity        = "com.tencent.wework.login.controller.LoginVeryfyStep2Activity"
	weComLaunchActivity           = "com.tencent.wework.launch.LaunchSplashActivity"
	weComLoginAgreementViewID     = DefaultWeComPackage + ":id/ow"
	androidImageViewClass         = "android.widget.ImageView"
	acceptPrivacyPolicyAction     = "accept_privacy_policy"
	acceptWeComLoginTermsAction   = "accept_wecom_login_terms"
	continueWeComWithWechatAction = "continue_wecom_with_wechat"
)

const authActionGenerationHexLength = sha256.Size * 2

type surfaceState struct {
	surface          core.Surface
	actions          map[string]surfaceActionState
	consumedActions  map[string]struct{}
	replayTombstones map[string]struct{}
	sequence         int64
	identity         surfaceIdentity
}

type surfaceActionState struct {
	advertised    core.Action
	companion     CompanionAction
	replayID      string
	nodeSignature string
	contextDigest string
	label         string
	bounds        Bounds
	sequence      int64
	identity      surfaceIdentity
}

type surfaceIdentity struct {
	packageName string
	windowID    int
	windowClass string
}

type sendMemo struct {
	digest string
	result core.SendResult
}

// Driver implements the shared driver boundary using only the official WeCom
// Android APK and the repository-owned accessibility companion.
type Driver struct {
	runtime *Runtime

	operationMu sync.Mutex
	mu          sync.Mutex
	account     core.AccountRuntime
	surfaces    map[string]surfaceState
	sendMemos   map[string]sendMemo
}

var _ core.Driver = (*Driver)(nil)
var _ core.AccountPurger = (*Driver)(nil)
var _ core.AuthActionDriver = (*Driver)(nil)

func New(config Config, executor Executor) (*Driver, error) {
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		return nil, err
	}
	return &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}, nil
}

func Factory(config Config, executor Executor) core.Factory {
	return func(_ core.AccountRuntime) (core.Driver, error) {
		return New(config, executor)
	}
}

func (d *Driver) Platform() core.Platform { return core.PlatformWeCom }

func (d *Driver) Start(ctx context.Context, account core.AccountRuntime) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if err := d.runtime.Start(ctx, account); err != nil {
		return err
	}
	d.mu.Lock()
	d.account = account
	d.surfaces = make(map[string]surfaceState)
	d.sendMemos = make(map[string]sendMemo)
	d.mu.Unlock()
	return nil
}

func (d *Driver) Stop(ctx context.Context) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if err := d.runtime.Stop(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	d.account = core.AccountRuntime{}
	d.surfaces = make(map[string]surfaceState)
	d.sendMemos = make(map[string]sendMemo)
	d.mu.Unlock()
	return nil
}

// Purge removes only an inactive container whose exact name and ownership
// labels match the requested account. Account files remain owned by the core
// account store, which deletes them after this hook succeeds.
func (d *Driver) Purge(ctx context.Context, account core.AccountRuntime) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	stateDir, err := validateAccountStateDir(account.StateDir, account.AccountID)
	if err != nil {
		return err
	}
	account.StateDir = stateDir
	if active, running := d.runtime.Account(); running {
		if active.AccountID == account.AccountID {
			return errors.New("cannot purge an active WeCom account")
		}
		return errors.New("cannot purge while another WeCom account runtime is active")
	}
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		return err
	}
	if strings.Contains(dataDir, ",") {
		return errors.New("account data path cannot contain a comma")
	}
	accountDir := filepath.Dir(dataDir)
	if err := ensureManagedDirectory(accountDir); err != nil {
		return fmt.Errorf("prepare WeCom account directory lock before purge: %w", err)
	}
	lock, err := acquireAccountLock(filepath.Join(accountDir, ".runtime.lock"))
	if err != nil {
		return fmt.Errorf("lock inactive WeCom account before purge: %w", err)
	}
	defer releaseAccountLock(lock)
	dataInfo, dataExists, err := inspectRealDirectory(dataDir)
	if err != nil {
		return fmt.Errorf("inspect WeCom Android data before purge: %w", err)
	}
	var data *profileDirectoryAnchor
	if dataExists {
		data, err = openProfileDirectoryWithoutSymlinks(dataDir)
		if err != nil {
			return fmt.Errorf("pin WeCom Android data before purge: %w", err)
		}
		defer data.close()
		expectedDevice, expectedInode, err := directoryIdentity(dataInfo)
		if err != nil {
			return err
		}
		actualDevice, actualInode, err := directoryIdentity(data.info)
		if err != nil || actualDevice != expectedDevice || actualInode != expectedInode {
			return errors.New("WeCom Android data changed before it could be pinned for purge")
		}
	}
	metadata, metadataExists, err := readWeComProfileMetadata(profileMetadataPath(account.StateDir))
	if err != nil {
		return fmt.Errorf("read WeCom profile metadata before purge: %w", err)
	}
	if metadataExists && data != nil {
		if err := validateWeComProfileMetadata(metadata, account.AccountID, data.info); err != nil {
			return fmt.Errorf("verify WeCom profile metadata before purge: %w", err)
		}
	}
	network := networkName(account.AccountID)
	networkID, networkExists, err := inspectAccountNetworkIdentity(
		ctx, d.runtime.executor, d.runtime.config.DockerBinary, network, account.AccountID,
	)
	if err != nil {
		return fmt.Errorf("verify isolated account network before purge: %w", err)
	}
	name := containerName(account.AccountID)
	containerEpoch, containerExists, err := inspectPurgeContainer(
		ctx, d.runtime.executor, d.runtime.config.DockerBinary,
		name, account.AccountID, d.runtime.config.RedroidImage, dataDir,
	)
	if err != nil {
		return err
	}
	if data != nil {
		device, inode, err := directoryIdentity(data.info)
		if err != nil {
			return err
		}
		if _, err := d.runtime.executor.RunInput(
			ctx, nil, 4096,
			d.runtime.config.DockerBinary,
			weComPurgeCleanupArgs(
				d.runtime.config.RedroidImage, dataDir, account.AccountID, device, inode,
			)...,
		); err != nil {
			return fmt.Errorf("clear root-owned account data in restricted cleanup container: %w", err)
		}
		entries, err := readPinnedProfileDirectoryAt(data)
		if err != nil {
			return fmt.Errorf("verify pinned cleared account data: %w", err)
		}
		if len(entries) != 0 {
			return errors.New("restricted cleanup container left pinned account data behind")
		}
		if err := verifyPinnedDirectoryCanonical(
			dataDir, data, "WeCom Android data changed during purge",
		); err != nil {
			return err
		}
	}
	if containerExists {
		verifiedEpoch, stillExists, err := inspectPurgeContainer(
			ctx, d.runtime.executor, d.runtime.config.DockerBinary,
			name, account.AccountID, d.runtime.config.RedroidImage, dataDir,
		)
		if err != nil || !stillExists {
			return fmt.Errorf("revalidate exact stopped WeCom container before purge removal: %w", err)
		}
		if !verifiedEpoch.equal(containerEpoch) {
			return errors.New("exact stopped WeCom container changed during purge")
		}
		if _, err := d.runtime.executor.Run(
			ctx, d.runtime.config.DockerBinary, "container", "rm", containerEpoch.ID,
		); err != nil {
			return fmt.Errorf("remove inactive WeCom container: %w", err)
		}
		_, remains, err := inspectPurgeContainer(
			ctx, d.runtime.executor, d.runtime.config.DockerBinary,
			name, account.AccountID, d.runtime.config.RedroidImage, dataDir,
		)
		if err != nil {
			return fmt.Errorf("verify exact WeCom container name after purge removal: %w", err)
		}
		if remains {
			return errors.New("WeCom container name changed during purge removal")
		}
	}
	if !networkExists {
		networkID = ""
	}
	return removeAccountNetwork(
		ctx, d.runtime.executor, d.runtime.config.DockerBinary,
		network, account.AccountID, networkID,
	)
}

func inspectPurgeContainer(
	ctx context.Context,
	executor Executor,
	dockerBinary, expectedName, accountID, image, dataDir string,
) (containerExecutionEpoch, bool, error) {
	out, inspectErr := executor.Run(ctx, dockerBinary, "container", "inspect", expectedName)
	if inspectErr != nil {
		listed, listErr := executor.Run(
			ctx, dockerBinary,
			"container", "ls", "--all", "--filter", "name=^/"+expectedName+"$", "--format", "{{.Names}}",
		)
		if listErr != nil {
			return containerExecutionEpoch{}, false, fmt.Errorf("inspect account container before purge: %w", inspectErr)
		}
		if strings.TrimSpace(string(listed)) == "" {
			return containerExecutionEpoch{}, false, nil
		}
		return containerExecutionEpoch{}, true, fmt.Errorf("inspect existing account container before purge: %w", inspectErr)
	}
	inspection, err := verifyPurgeContainer(out, expectedName, accountID, image, dataDir)
	if err != nil {
		return containerExecutionEpoch{}, true, err
	}
	epoch, err := stoppedContainerExecutionEpoch(inspection)
	if err != nil {
		return containerExecutionEpoch{}, true, fmt.Errorf("refusing to purge a container that is not exactly stopped: %w", err)
	}
	return epoch, true, nil
}

func verifyPurgeContainer(
	raw []byte,
	expectedName, accountID, image, dataDir string,
) (runtimeContainerInspection, error) {
	var inspections []runtimeContainerInspection
	if err := json.Unmarshal(raw, &inspections); err != nil || len(inspections) != 1 {
		return runtimeContainerInspection{}, errors.New("cannot decode a unique container inspection before purge")
	}
	inspection := inspections[0]
	if inspection.Name != "/"+expectedName ||
		inspection.Config.Labels[labelDriver] != "wecom" ||
		inspection.Config.Labels[labelAccount] != accountID ||
		inspection.Config.Image != image ||
		inspection.Config.Hostname != containerHostname(accountID) {
		return runtimeContainerInspection{}, errors.New("refusing to purge a container without exact WeCom account ownership, name, image, and hostname")
	}
	expectedSource := canonicalPath(dataDir)
	matched := 0
	for _, mount := range inspection.Mounts {
		if mount.Destination != "/data" {
			continue
		}
		matched++
		if mount.Type != "bind" || !mount.RW || canonicalPath(mount.Source) != expectedSource {
			return runtimeContainerInspection{}, errors.New("refusing to purge a container whose /data bind mount does not exactly match the account")
		}
	}
	if matched != 1 {
		return runtimeContainerInspection{}, errors.New("refusing to purge a container without exactly one verified /data bind mount")
	}
	return inspection, nil
}

func weComPurgeCleanupArgs(
	image, dataDir, accountID string,
	expectedDevice, expectedInode uint64,
) []string {
	const verifyAndClear = `actual=$(/system/bin/toybox stat -Lc %d:%i /account-data) || exit 71
[ "$actual" = "$1:$2" ] || exit 72
/system/bin/toybox rm -rf /account-data/* /account-data/.[!.]* /account-data/..?*`
	return []string{
		"container", "run", "--rm", "--pull", "never",
		"--network", "none", "--read-only", "--pids-limit", "32", "--memory", "64m",
		"--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER",
		"--security-opt", "no-new-privileges=true", "--user", "0:0",
		"--label", labelDriver + "=wecom-purge", "--label", labelAccount + "=" + accountID,
		"--mount", "type=bind,src=" + dataDir + ",dst=/account-data",
		"--entrypoint", "/system/bin/toybox",
		image, "sh", "-c", verifyAndClear, "wechatcopilot-purge",
		strconv.FormatUint(expectedDevice, 10), strconv.FormatUint(expectedInode, 10),
	}
}

func canonicalPath(path string) string {
	cleaned := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err == nil {
		return resolved
	}
	return cleaned
}

func (d *Driver) Status(ctx context.Context) (core.Status, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	observed := time.Now().UTC()
	client, err := d.runtime.Companion()
	if err != nil {
		return core.Status{State: core.StateStopped, ObservedAt: observed, Capabilities: capabilities()}, nil
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return core.Status{
			State:        core.StateOffline,
			Reason:       "accessibility companion is unavailable",
			ObservedAt:   observed,
			Capabilities: capabilities(),
		}, nil
	}
	snapshot = d.withForegroundActivity(ctx, snapshot)
	state, reason := classifyLogin(snapshot)
	return core.Status{
		State:         state,
		Reason:        reason,
		ClientVersion: d.runtime.ClientVersion(),
		ObservedAt:    observed,
		Capabilities:  capabilities(),
	}, nil
}

func capabilities() map[string]core.Support {
	return core.CapabilityMap(map[string]core.Support{
		core.CapabilityAuthQR:          core.SupportExperimental,
		core.CapabilityAuthSMS:         core.SupportExperimental,
		core.CapabilityMessagesWatch:   core.SupportExperimental,
		core.CapabilityMessagesSend:    core.SupportExperimental,
		core.CapabilityWebOpen:         core.SupportExperimental,
		core.CapabilityMiniProgramOpen: core.SupportExperimental,
		core.CapabilitySurfaceAct:      core.SupportExperimental,
	})
}

func (d *Driver) AuthSnapshot(ctx context.Context) (core.AuthSnapshot, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	client, err := d.runtime.Companion()
	if err != nil {
		return core.AuthSnapshot{}, err
	}
	android, err := d.runtime.Android()
	if err != nil {
		return core.AuthSnapshot{}, err
	}
	_, result, err := d.captureAuthSnapshot(ctx, client, android)
	return result, err
}

// captureAuthSnapshot sandwiches one screenshot between two observations of
// the same official window. Any advertised action is derived from the second
// observation and the exact PNG returned with it.
func (d *Driver) captureAuthSnapshot(
	ctx context.Context,
	client *CompanionClient,
	android AndroidContainer,
) (UISnapshot, core.AuthSnapshot, error) {
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return UISnapshot{}, core.AuthSnapshot{}, err
	}
	snapshot = withForegroundActivity(ctx, android, snapshot)
	d.mu.Lock()
	accountID := d.account.AccountID
	d.mu.Unlock()
	result := describeAuthSnapshot(snapshot, nil, accountID)
	if !authScreenshotWindow(snapshot) {
		return snapshot, result, nil
	}
	png, err := android.Screenshot(ctx)
	if err != nil {
		return UISnapshot{}, core.AuthSnapshot{}, err
	}
	verified, err := client.Snapshot(ctx)
	if err != nil {
		return UISnapshot{}, core.AuthSnapshot{}, err
	}
	verified = withForegroundActivity(ctx, android, verified)
	verifiedResult := describeAuthSnapshot(verified, nil, accountID)
	if !authScreenshotWindow(verified) {
		if verifiedResult.State == core.StateOnline {
			return verified, verifiedResult, nil
		}
		if verified.PackageName != DefaultWeComPackage {
			return UISnapshot{}, core.AuthSnapshot{}, fmt.Errorf("%w: authentication image left the official WeCom package", ErrTargetAmbiguous)
		}
		return UISnapshot{}, core.AuthSnapshot{}, fmt.Errorf("%w: authentication image window changed before verification", ErrStale)
	}
	if !sameAuthenticationObservation(snapshot, verified) {
		return UISnapshot{}, core.AuthSnapshot{}, fmt.Errorf("%w: authentication image changed during capture", ErrStale)
	}
	verifiedResult = describeAuthSnapshot(verified, png, accountID)
	verifiedResult.ScreenshotPNG = png
	if verifiedResult.Kind == core.AuthQR {
		// The full login frame is deliberate: cropping an unverified visual
		// region could present the wrong QR code to the user.
		verifiedResult.QRCodePNG = png
	}
	return verified, verifiedResult, nil
}

func describeAuthSnapshot(snapshot UISnapshot, screenshotPNG []byte, accountID string) core.AuthSnapshot {
	state, prompt := classifyLogin(snapshot)
	kind := core.AuthPhoneConfirm
	canSubmit := false
	var actions []core.AuthAction
	actionPrompt := ""
	if authScreenshotWindow(snapshot) {
		actions, actionPrompt = authenticationActions(snapshot, screenshotPNG, accountID)
	}
	if len(actions) != 0 {
		state = core.StateAuthRequired
		prompt = actionPrompt
	}
	text := snapshotText(snapshot)
	if authScreenshotWindow(snapshot) && validSMSAuthSnapshot(snapshot) {
		kind = core.AuthSMS
		canSubmit = true
	} else if state == core.StateAuthRequired && containsAny(text, "二维码", "扫码", "scan") {
		kind = core.AuthQR
	}
	return core.AuthSnapshot{
		Kind:          kind,
		State:         state,
		Prompt:        prompt,
		CanSubmitCode: canSubmit,
		Actions:       actions,
		ObservedAt:    time.Now().UTC(),
	}
}

func authScreenshotWindow(snapshot UISnapshot) bool {
	if snapshot.PackageName != DefaultWeComPackage || snapshot.Sequence <= 0 || snapshot.WindowID < 0 ||
		strings.TrimSpace(snapshot.WindowClass) == "" || !authenticationSurface(snapshot) {
		return false
	}
	return isWeComActivity(snapshot, weComLoginWxAuthActivity) ||
		isWeComActivity(snapshot, weComSMSVerifyActivity) || isWeComActivity(snapshot, weComLaunchActivity)
}

func (d *Driver) PerformAuthAction(ctx context.Context, request core.AuthActionRequest) error {
	operation, ok := parseAuthActionID(request.ActionID)
	if !ok {
		return fmt.Errorf("%w: authentication action is not advertised", ErrStale)
	}
	if !request.Confirmed {
		return fmt.Errorf("%w: authentication action requires explicit user confirmation", ErrUserActionRequired)
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	client, err := d.runtime.Companion()
	if err != nil {
		return err
	}
	android, err := d.runtime.Android()
	if err != nil {
		return err
	}
	snapshot, auth, err := d.captureAuthSnapshot(ctx, client, android)
	if err != nil {
		return err
	}
	if !exactImageBoundAuthAction(auth, request.ActionID) {
		return fmt.Errorf("%w: authentication action generation is stale", ErrStale)
	}
	if snapshotRequiresUserAction(snapshot) {
		return ErrUserActionRequired
	}
	switch operation {
	case acceptPrivacyPolicyAction:
		target, targetErr := uniquePrivacyConsentTarget(snapshot)
		if targetErr != nil {
			return targetErr
		}
		if _, err = client.Act(ctx, CompanionAction{
			Kind: ActionClick, NodeID: target.ID, ExpectedSequence: snapshot.Sequence,
		}); err != nil {
			return markUncertainAuthActionConsumed(err)
		}
		if waitErr := waitForPrivacyConsentDismissal(ctx, client, android, snapshot.Sequence); waitErr != nil {
			return core.MarkAuthActionConsumed(waitErr)
		}
		return nil
	case acceptWeComLoginTermsAction:
		targets, targetErr := weComLoginMethodTargets(snapshot)
		if targetErr != nil {
			return targetErr
		}
		if weComLoginTermsAccepted(targets.terms) {
			return fmt.Errorf("%w: official WeCom login terms are already accepted", ErrStale)
		}
		if _, err = client.Act(ctx, CompanionAction{
			Kind: ActionCheck, NodeID: targets.terms.ID, ExpectedSequence: snapshot.Sequence,
		}); err != nil {
			return markUncertainAuthActionConsumed(err)
		}
		if waitErr := waitForWeComLoginTermsAccepted(ctx, client, android, snapshot.Sequence); waitErr != nil {
			return core.MarkAuthActionConsumed(waitErr)
		}
		return nil
	case continueWeComWithWechatAction:
		targets, targetErr := weComLoginMethodTargets(snapshot)
		if targetErr != nil {
			return targetErr
		}
		if !weComLoginTermsAccepted(targets.terms) {
			return fmt.Errorf("%w: official WeCom login terms are not accepted", ErrStale)
		}
		if _, err = client.Act(ctx, CompanionAction{
			Kind: ActionClick, NodeID: targets.wechat.ID, ExpectedSequence: snapshot.Sequence,
		}); err != nil {
			return markUncertainAuthActionConsumed(err)
		}
		if waitErr := waitForWeComLoginMethodDismissal(ctx, client, android, snapshot.Sequence); waitErr != nil {
			return core.MarkAuthActionConsumed(waitErr)
		}
		return nil
	default:
		return fmt.Errorf("%w: authentication action is not advertised", ErrStale)
	}
}

func exactImageBoundAuthAction(snapshot core.AuthSnapshot, actionID string) bool {
	return snapshot.State == core.StateAuthRequired && len(snapshot.Actions) == 1 &&
		snapshot.Actions[0].ID == actionID && snapshot.Actions[0].ImageBound
}

func markUncertainAuthActionConsumed(err error) error {
	if errors.Is(err, ErrActionOutcomeUncertain) {
		return core.MarkAuthActionConsumed(err)
	}
	return err
}

func (d *Driver) withForegroundActivity(ctx context.Context, snapshot UISnapshot) UISnapshot {
	android, err := d.runtime.Android()
	if err != nil {
		return snapshot
	}
	return withForegroundActivity(ctx, android, snapshot)
}

func withForegroundActivity(ctx context.Context, android AndroidContainer, snapshot UISnapshot) UISnapshot {
	if snapshot.PackageName != DefaultWeComPackage {
		return snapshot
	}
	// The resumed Activity is authoritative for state classification. Clear a
	// companion observation first so a failed probe cannot reuse a stale class.
	snapshot.WindowClass = ""
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	activity, err := android.ForegroundActivity(probeCtx, DefaultWeComPackage)
	if err == nil {
		snapshot.WindowClass = activity
	}
	return snapshot
}

func (d *Driver) SubmitAuthCode(ctx context.Context, code string) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	code = strings.TrimSpace(code)
	if len(code) < 4 || len(code) > 8 {
		return errors.New("verification code must contain 4 to 8 digits")
	}
	for _, char := range code {
		if char < '0' || char > '9' {
			return errors.New("verification code must contain digits only")
		}
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return err
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return err
	}
	android, err := d.runtime.Android()
	if err != nil {
		return err
	}
	snapshot = withForegroundActivity(ctx, android, snapshot)
	if !validSMSAuthSnapshot(snapshot) {
		return fmt.Errorf("%w: current official screen does not expose a unique SMS verification input", ErrStale)
	}
	editable := editableNodes(snapshot)
	if _, err := client.Act(ctx, CompanionAction{Kind: ActionSetText, NodeID: editable[0].ID, Text: code, ExpectedSequence: snapshot.Sequence}); err != nil {
		return err
	}
	updated, err := waitForSnapshotChange(ctx, client, snapshot.Sequence)
	if err != nil {
		return err
	}
	updated = withForegroundActivity(ctx, android, updated)
	if !validSMSAuthSnapshot(updated) {
		return fmt.Errorf("%w: official SMS verification screen changed before submission", ErrStale)
	}
	button, err := uniqueNode(updated, func(node Node) bool {
		return node.Clickable && node.Enabled && node.VisibleToUser && usableBounds(node.Bounds) &&
			matchesAny(nodeLabel(node), "确定", "登录", "下一步", "验证", "submit", "continue")
	})
	if err != nil {
		return fmt.Errorf("submit verification code: %w", err)
	}
	_, err = client.Act(ctx, CompanionAction{Kind: ActionClick, NodeID: button.ID, ExpectedSequence: updated.Sequence})
	return err
}

func (d *Driver) ListConversations(ctx context.Context, query core.ConversationQuery) ([]core.Conversation, error) {
	client, err := d.runtime.Companion()
	if err != nil {
		return nil, err
	}
	history, err := allEvents(ctx, client)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	accountID := d.account.AccountID
	d.mu.Unlock()
	byID := make(map[string]core.Conversation)
	for _, event := range history.Events {
		title := event.Conversation
		if title == "" {
			title = event.Title
		}
		if event.PackageName != d.runtime.config.WeComPackage {
			continue
		}
		if !sha256Pattern.MatchString(event.ConversationKey) {
			return nil, fmt.Errorf("%w: companion event lacks a stable conversation identity", ErrClientIncompatible)
		}
		if title == "" {
			continue
		}
		if query.Search != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(query.Search)) {
			continue
		}
		id := conversationID(accountID, event.ConversationKey)
		conversation := byID[id]
		conversation.ID = id
		conversation.ExternalID = id
		conversation.Title = title
		conversation.Kind = "unknown"
		conversation.UnreadCount = 1
		conversation.Complete = false
		conversation.Source = "android_notification"
		if event.PostedAt.After(conversation.LastMessageAt) {
			conversation.LastMessageAt = event.PostedAt
		}
		byID[id] = conversation
	}
	result := make([]core.Conversation, 0, len(byID))
	for _, conversation := range byID {
		if query.Unread && conversation.UnreadCount == 0 {
			continue
		}
		result = append(result, conversation)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LastMessageAt.After(result[j].LastMessageAt) })
	limit := query.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (d *Driver) ReadMessages(ctx context.Context, query core.MessageQuery) ([]core.Message, error) {
	client, err := d.runtime.Companion()
	if err != nil {
		return nil, err
	}
	limit := query.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	page, err := client.Events(ctx, query.AfterSequence, limit)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	accountID := d.account.AccountID
	d.mu.Unlock()
	messages := make([]core.Message, 0, len(page.Events))
	// An older companion or a page whose global cursor predates the ring cannot
	// prove continuity for any conversation first observed on this page. New
	// companions additionally carry event.GapBefore so the boundary survives
	// later global pages and conversation filtering.
	pageBoundaryApplied := make(map[string]bool)
	pendingConversationGap := make(map[string]bool)
	for _, event := range page.Events {
		if event.PackageName != d.runtime.config.WeComPackage {
			continue
		}
		if !sha256Pattern.MatchString(event.ConversationKey) {
			return nil, fmt.Errorf("%w: companion event lacks a stable conversation identity", ErrClientIncompatible)
		}
		title := event.Conversation
		if title == "" {
			title = event.Title
		}
		conversation := conversationID(accountID, event.ConversationKey)
		if event.GapBefore {
			pendingConversationGap[conversation] = true
		}
		if query.ConversationID != "" && query.ConversationID != conversation {
			continue
		}
		if !query.Before.IsZero() && !event.PostedAt.Before(query.Before) {
			continue
		}
		id := messageID(accountID, event.Sequence)
		surfaceRef := ""
		if event.Openable {
			surfaceRef = notificationSurfaceReference(accountID, event.Sequence)
		}
		gapBefore := pendingConversationGap[conversation]
		if !page.Complete && !pageBoundaryApplied[conversation] {
			gapBefore = true
			pageBoundaryApplied[conversation] = true
		}
		if gapBefore {
			delete(pendingConversationGap, conversation)
		}
		messages = append(messages, core.Message{
			ID:             id,
			ExternalID:     id,
			ConversationID: conversation,
			SenderName:     event.Sender,
			SentAt:         event.PostedAt,
			Kind:           "text",
			Text:           event.Text,
			SurfaceRef:     surfaceRef,
			Source:         "android_notification",
			Complete:       false,
			Confidence:     0.7,
			Sequence:       event.Sequence,
			GapBefore:      gapBefore,
		})
	}
	return messages, nil
}

func (d *Driver) Send(ctx context.Context, request core.SendRequest) (core.SendResult, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if request.ConversationID == "" || request.IdempotencyKey == "" {
		return core.SendResult{}, errors.New("conversation ID and idempotency key are required")
	}
	if request.Text == "" || len(request.Attachments) != 0 || request.ShareSurfaceID != "" {
		return core.SendResult{}, fmt.Errorf("%w: v0 WeCom driver sends text only", ErrUnsupportedCapability)
	}
	digest := wecomSendDigest(request)
	d.mu.Lock()
	memo, exists := d.sendMemos[request.IdempotencyKey]
	d.mu.Unlock()
	if exists {
		if memo.digest != digest {
			return core.SendResult{}, errors.New("idempotency key was already used for different content")
		}
		return memo.result, nil
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return core.SendResult{}, err
	}
	event, err := d.resolveConversationEvent(ctx, request.ConversationID)
	if err != nil {
		return core.SendResult{}, err
	}
	frame, err := d.openNotificationConversation(ctx, client, event)
	if err != nil {
		return core.SendResult{}, err
	}
	baseline := len(outgoingBubbleEvidence(frame.snapshot, request.Text, frame.binding))
	if _, err := client.Act(ctx, CompanionAction{
		Kind: ActionSetText, NodeID: frame.composer.ID, Text: request.Text,
		ExpectedSequence: frame.snapshot.Sequence,
	}); err != nil {
		return core.SendResult{}, err
	}
	updated, err := waitForSnapshotChange(ctx, client, frame.snapshot.Sequence)
	if err != nil {
		return core.SendResult{}, err
	}
	android, err := d.runtime.Android()
	if err != nil {
		return core.SendResult{}, err
	}
	updated = withForegroundActivity(ctx, android, updated)
	prepared, err := validateChatFrame(updated, eventConversationTitle(event), &frame.binding, true)
	if err != nil {
		return core.SendResult{}, fmt.Errorf("revalidate message composer after setting text: %w", err)
	}
	if prepared.composer.Text != request.Text {
		return core.SendResult{}, fmt.Errorf("%w: message composer did not contain the exact requested text", ErrStale)
	}

	// Capture the exact semantic tree a second time immediately before the
	// external write. This also arms the companion's one-shot snapshot guard
	// for this precise sequence and prevents a stale prepared frame from being
	// used if a dialog, conversation, or control changed in the meantime.
	verified, err := client.Snapshot(ctx)
	if err != nil {
		return core.SendResult{}, err
	}
	verified = withForegroundActivity(ctx, android, verified)
	preparedAgain, err := validateChatFrame(verified, eventConversationTitle(event), &frame.binding, true)
	if err != nil {
		return core.SendResult{}, fmt.Errorf("revalidate message composer before send: %w", err)
	}
	if preparedAgain.snapshot.Sequence != prepared.snapshot.Sequence ||
		surfaceContextDigest(preparedAgain.snapshot) != surfaceContextDigest(prepared.snapshot) ||
		preparedAgain.composer.Text != request.Text || preparedAgain.send != prepared.send {
		return core.SendResult{}, fmt.Errorf("%w: prepared message frame changed before send", ErrStale)
	}

	if _, err := client.Act(ctx, CompanionAction{
		Kind: ActionClick, NodeID: preparedAgain.send.ID,
		ExpectedSequence: preparedAgain.snapshot.Sequence,
	}); err != nil {
		if !errors.Is(err, ErrActionOutcomeUncertain) {
			return core.SendResult{}, err
		}
		result := core.SendResult{Uncertain: true, Detail: "send action may have reached the official client, but its result was not observed"}
		d.rememberSend(request.IdempotencyKey, digest, result)
		return result, nil
	}
	confirmed, verifyErr := waitForDirectionalOutgoingBubble(
		ctx, client, android, eventConversationTitle(event), frame.binding,
		request.Text, baseline, preparedAgain.snapshot.Sequence, 8*time.Second,
	)
	if verifyErr != nil || !confirmed {
		result := core.SendResult{Uncertain: true, Detail: ErrSendUncertain.Error()}
		d.rememberSend(request.IdempotencyKey, digest, result)
		return result, nil
	}
	result := core.SendResult{
		MessageID: "wecom-ui-" + digestID(request.IdempotencyKey),
		Verified:  true,
		Detail:    "a stable new right-aligned outbound bubble was observed in the bound official conversation",
	}
	d.rememberSend(request.IdempotencyKey, digest, result)
	return result, nil
}

func (d *Driver) rememberSend(key, digest string, result core.SendResult) {
	d.mu.Lock()
	d.sendMemos[key] = sendMemo{digest: digest, result: result}
	d.mu.Unlock()
}

func wecomSendDigest(request core.SendRequest) string {
	request.IdempotencyKey = ""
	encoded, _ := json.Marshal(request)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (d *Driver) OpenSurface(ctx context.Context, reference string) (core.Surface, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return core.Surface{}, errors.New("surface reference is required")
	}
	payload, ok := strings.CutPrefix(reference, "wecom-notification:")
	if !ok {
		return core.Surface{}, fmt.Errorf("%w: v0 opens only message-backed WeCom notification references", ErrUnsupportedCapability)
	}
	d.mu.Lock()
	accountID := d.account.AccountID
	d.mu.Unlock()
	sequenceText, scope, ok := strings.Cut(payload, ":")
	sequence, err := strconv.ParseInt(sequenceText, 10, 64)
	if !ok || strings.Contains(scope, ":") || err != nil || sequence <= 0 ||
		strconv.FormatInt(sequence, 10) != sequenceText || accountID == "" ||
		scope != notificationSurfaceScope(accountID, sequence) {
		return core.Surface{}, fmt.Errorf("%w: notification surface reference is invalid for the active account", ErrNotFound)
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return core.Surface{}, err
	}
	event, err := d.notificationEvent(ctx, client, sequence)
	if err != nil {
		return core.Surface{}, err
	}
	if !event.Openable {
		return core.Surface{}, fmt.Errorf("%w: notification no longer has an openable PendingIntent", ErrStale)
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return core.Surface{}, err
	}
	if _, err := client.Act(ctx, CompanionAction{Kind: ActionOpenNotification, NodeID: sequenceText}); err != nil {
		return core.Surface{}, fmt.Errorf("%w: open notification surface: %w", ErrStale, err)
	}
	opened, err := waitForSnapshotChange(ctx, client, snapshot.Sequence)
	if err != nil {
		return core.Surface{}, err
	}
	return d.recordSurface(ctx, opened)
}

func (d *Driver) notificationEvent(ctx context.Context, client *CompanionClient, sequence int64) (CompanionEvent, error) {
	history, err := allEvents(ctx, client)
	if err != nil {
		return CompanionEvent{}, err
	}
	var matches []CompanionEvent
	for _, event := range history.Events {
		if event.Sequence == sequence && event.PackageName == d.runtime.config.WeComPackage {
			matches = append(matches, event)
		}
	}
	if len(matches) == 0 {
		if !history.Complete {
			return CompanionEvent{}, fmt.Errorf("%w: notification sequence may precede a gap in the bounded journal", ErrStale)
		}
		return CompanionEvent{}, fmt.Errorf("%w: notification sequence is absent from the bounded journal", ErrNotFound)
	}
	if len(matches) != 1 {
		return CompanionEvent{}, ErrTargetAmbiguous
	}
	return matches[0], nil
}

func (d *Driver) SnapshotSurface(ctx context.Context, surfaceID string) (core.Surface, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	state, exists := d.surfaces[surfaceID]
	d.mu.Unlock()
	if !exists {
		return core.Surface{}, fmt.Errorf("%w: unknown or closed surface", ErrNotFound)
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return core.Surface{}, err
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return core.Surface{}, err
	}
	return d.updateSurface(ctx, surfaceID, snapshot, state.identity)
}

func (d *Driver) ActSurface(ctx context.Context, surfaceID string, action core.SurfaceAction) (core.Surface, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	state, exists := d.surfaces[surfaceID]
	boundAction, allowed := state.actions[action.ActionID]
	d.mu.Unlock()
	if !exists {
		return core.Surface{}, fmt.Errorf("%w: unknown or closed surface", ErrNotFound)
	}
	if !allowed {
		return core.Surface{}, fmt.Errorf("%w: surface action was not advertised", ErrStale)
	}
	risk := strings.ToLower(strings.TrimSpace(boundAction.advertised.Risk))
	effect := strings.ToLower(strings.TrimSpace(boundAction.advertised.Effect))
	if boundAction.advertised.Disabled || risk == "high" || risk == "sensitive" || risk == "destructive" ||
		effect == "high_risk" || effect == "sensitive" || effect == "destructive" {
		return core.Surface{}, ErrUserActionRequired
	}
	if !action.TextProvided && action.Text != "" {
		return core.Surface{}, fmt.Errorf("%w: surface action text presence is inconsistent", ErrInvalidArgument)
	}
	companionAction := boundAction.companion
	if companionAction.Kind == ActionSetText {
		if !action.TextProvided {
			return core.Surface{}, fmt.Errorf("%w: set-text requires an explicitly provided text value", ErrInvalidArgument)
		}
		companionAction.Text = action.Text
	} else if action.TextProvided || action.Text != "" {
		return core.Surface{}, fmt.Errorf("%w: this surface action does not accept text", ErrInvalidArgument)
	}
	if action.TextProvided && (utf8.RuneCountInString(action.Text) > 4_096 || strings.ContainsRune(action.Text, '\x00')) {
		return core.Surface{}, fmt.Errorf("%w: surface input text must not exceed 4096 characters or contain NUL", ErrInvalidArgument)
	}
	if (risk != "low" || effect == "external_write") && !action.Confirmed {
		return core.Surface{}, ErrConfirmationRequired
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return core.Surface{}, err
	}
	before, err := client.Snapshot(ctx)
	if err != nil {
		return core.Surface{}, err
	}
	before, _, err = d.prepareSurfaceSnapshot(ctx, before)
	if err != nil {
		return core.Surface{}, err
	}
	if !state.identity.matches(before) {
		return core.Surface{}, fmt.Errorf("%w: surface window identity changed before action", ErrStale)
	}
	if before.Sequence != state.sequence {
		return core.Surface{}, fmt.Errorf("%w: surface changed since the action was advertised", ErrStale)
	}
	if err := validateSurfaceActionState(surfaceID, before, boundAction); err != nil {
		return core.Surface{}, err
	}
	companionAction.ExpectedSequence = before.Sequence
	if !d.consumeSurfaceAction(surfaceID, action.ActionID, boundAction) {
		return core.Surface{}, fmt.Errorf("%w: surface action was already consumed", ErrStale)
	}
	if shouldTombstoneSurfaceReplay(effect, true) && effect != "navigate" {
		d.tombstoneSurfaceReplay(surfaceID, boundAction.replayID)
	}
	if _, err := client.Act(ctx, companionAction); err != nil {
		if shouldTombstoneSurfaceReplay(effect, false) {
			d.tombstoneSurfaceReplay(surfaceID, boundAction.replayID)
		}
		return core.Surface{}, err
	}
	after, err := waitForSnapshotChange(ctx, client, before.Sequence)
	if err != nil {
		if shouldTombstoneSurfaceReplay(effect, false) {
			d.tombstoneSurfaceReplay(surfaceID, boundAction.replayID)
		}
		return core.Surface{}, err
	}
	result, err := d.updateSurface(ctx, surfaceID, after, state.identity)
	if err != nil && shouldTombstoneSurfaceReplay(effect, false) {
		d.tombstoneSurfaceReplay(surfaceID, boundAction.replayID)
	}
	return result, err
}

func (d *Driver) CloseSurface(ctx context.Context, surfaceID string) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	state, exists := d.surfaces[surfaceID]
	d.mu.Unlock()
	if !exists {
		return fmt.Errorf("%w: unknown or closed surface", ErrNotFound)
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return err
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return err
	}
	snapshot, _, err = d.prepareSurfaceSnapshot(ctx, snapshot)
	if err != nil {
		return err
	}
	if !state.identity.matches(snapshot) {
		return fmt.Errorf("%w: surface window identity changed before close", ErrStale)
	}
	if snapshot.Sequence != state.sequence {
		return fmt.Errorf("%w: surface changed since close was requested", ErrStale)
	}
	if _, err := client.Act(ctx, CompanionAction{
		Kind: ActionGlobalBack, ExpectedSequence: snapshot.Sequence,
	}); err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.surfaces, surfaceID)
	d.mu.Unlock()
	return nil
}

func (d *Driver) resolveConversationEvent(ctx context.Context, id string) (CompanionEvent, error) {
	client, err := d.runtime.Companion()
	if err != nil {
		return CompanionEvent{}, err
	}
	history, err := allEvents(ctx, client)
	if err != nil {
		return CompanionEvent{}, err
	}
	d.mu.Lock()
	accountID := d.account.AccountID
	d.mu.Unlock()
	var matches []CompanionEvent
	for _, event := range history.Events {
		if event.PackageName == d.runtime.config.WeComPackage && sha256Pattern.MatchString(event.ConversationKey) && conversationID(accountID, event.ConversationKey) == id {
			matches = append(matches, event)
		}
	}
	if len(matches) == 0 {
		if !history.Complete {
			return CompanionEvent{}, fmt.Errorf("%w: conversation may precede a gap in the bounded notification journal", ErrStale)
		}
		return CompanionEvent{}, fmt.Errorf("%w: conversation ID is not present in the bounded notification journal", ErrNotFound)
	}
	title := matches[0].Conversation
	if title == "" {
		title = matches[0].Title
	}
	var latest CompanionEvent
	for _, event := range matches {
		eventTitle := event.Conversation
		if eventTitle == "" {
			eventTitle = event.Title
		}
		if eventTitle == "" || eventTitle != title {
			return CompanionEvent{}, fmt.Errorf("%w: one notification identity maps to multiple conversation titles", ErrTargetAmbiguous)
		}
		if event.Openable && event.Sequence > latest.Sequence {
			latest = event
		}
	}
	if latest.Sequence <= 0 {
		return CompanionEvent{}, fmt.Errorf("%w: conversation has no verified notification PendingIntent", ErrTargetAmbiguous)
	}
	return latest, nil
}

func (d *Driver) openNotificationConversation(ctx context.Context, client *CompanionClient, event CompanionEvent) (chatFrame, error) {
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return chatFrame{}, err
	}
	sequence := strconv.FormatInt(event.Sequence, 10)
	if _, err := client.Act(ctx, CompanionAction{Kind: ActionOpenNotification, NodeID: sequence}); err != nil {
		return chatFrame{}, fmt.Errorf("%w: open verified conversation notification: %w", ErrStale, err)
	}
	opened, err := waitForSnapshotChange(ctx, client, snapshot.Sequence)
	if err != nil {
		return chatFrame{}, err
	}
	android, err := d.runtime.Android()
	if err != nil {
		return chatFrame{}, err
	}
	opened = withForegroundActivity(ctx, android, opened)
	frame, err := validateChatFrame(opened, eventConversationTitle(event), nil, false)
	if err != nil {
		return chatFrame{}, err
	}
	// Require a second identical semantic observation before returning a frame
	// that can be used to prepare an external write.
	stable, err := client.Snapshot(ctx)
	if err != nil {
		return chatFrame{}, err
	}
	stable = withForegroundActivity(ctx, android, stable)
	stableFrame, err := validateChatFrame(stable, eventConversationTitle(event), &frame.binding, false)
	if err != nil {
		return chatFrame{}, err
	}
	if stableFrame.snapshot.Sequence != frame.snapshot.Sequence ||
		surfaceContextDigest(stableFrame.snapshot) != surfaceContextDigest(frame.snapshot) {
		return chatFrame{}, fmt.Errorf("%w: opened conversation changed before send preparation", ErrStale)
	}
	return stableFrame, nil
}

func eventConversationTitle(event CompanionEvent) string {
	if strings.TrimSpace(event.Conversation) != "" {
		return strings.TrimSpace(event.Conversation)
	}
	return strings.TrimSpace(event.Title)
}

func (d *Driver) recordSurface(ctx context.Context, snapshot UISnapshot) (core.Surface, error) {
	d.mu.Lock()
	accountID := d.account.AccountID
	d.mu.Unlock()
	id := "surface-" + digestID(accountID+":"+strconv.FormatInt(time.Now().UnixNano(), 10))
	return d.updateSurface(ctx, id, snapshot, surfaceIdentity{})
}

func (d *Driver) updateSurface(
	ctx context.Context,
	id string,
	snapshot UISnapshot,
	bound surfaceIdentity,
) (core.Surface, error) {
	snapshot, android, err := d.prepareSurfaceSnapshot(ctx, snapshot)
	if err != nil {
		return core.Surface{}, err
	}
	if bound.valid() && !bound.matches(snapshot) {
		return core.Surface{}, fmt.Errorf("%w: surface window identity changed", ErrStale)
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return core.Surface{}, err
	}
	png, err := android.Screenshot(ctx)
	if err != nil {
		return core.Surface{}, err
	}
	verified, err := client.Snapshot(ctx)
	if err != nil {
		return core.Surface{}, err
	}
	verified, _, err = d.prepareSurfaceSnapshot(ctx, verified)
	if err != nil {
		return core.Surface{}, err
	}
	if !sameSurfaceObservation(snapshot, verified) {
		return core.Surface{}, fmt.Errorf("%w: surface changed during screenshot capture", ErrStale)
	}
	snapshot = verified
	identity := identityForSurface(snapshot)
	contextDigest := surfaceContextDigest(snapshot)
	d.mu.Lock()
	previous, hasPrevious := d.surfaces[id]
	consumedActions := make(map[string]struct{})
	replayTombstones := make(map[string]struct{})
	if hasPrevious && previous.identity == identity {
		for actionID := range previous.consumedActions {
			consumedActions[actionID] = struct{}{}
		}
		for replayID := range previous.replayTombstones {
			replayTombstones[replayID] = struct{}{}
		}
	}
	d.mu.Unlock()
	actions := make([]core.Action, 0)
	allowed := make(map[string]surfaceActionState)
	highRisk := surfaceHighRisk(snapshot) || authenticationSurface(snapshot)
	for _, node := range snapshot.Nodes {
		if node.ID == "" || !node.VisibleToUser || !validNodeBounds(node.Bounds) {
			continue
		}
		var kinds []string
		if node.Editable && node.Enabled {
			kinds = append(kinds, ActionSetText)
		}
		if node.Clickable && node.Enabled {
			kinds = append(kinds, ActionClick)
		}
		if node.Scrollable && node.Enabled {
			kinds = append(kinds, ActionScrollForward, ActionScrollBackward)
		}
		for _, kind := range kinds {
			actionLabel := surfaceActionLabel(snapshot, node, kind)
			risk, effect := classifySurfaceAction(snapshot, node, kind)
			agreementClick := kind == ActionClick && isWeComLoginAgreementImage(node)
			if highRisk || agreementClick {
				risk, effect = "high", "high_risk"
			}
			boundAction := bindSurfaceAction(id, snapshot, node, kind, actionLabel, risk, effect, contextDigest)
			actionID := boundAction.advertised.ID
			if _, consumed := consumedActions[actionID]; consumed {
				continue
			}
			if _, tombstoned := replayTombstones[boundAction.replayID]; tombstoned {
				continue
			}
			allowed[actionID] = boundAction
			action := boundAction.advertised
			actions = append(actions, action)
		}
	}
	screenshotDigest := sha256.Sum256(png)
	surface := core.Surface{
		ID: id, Kind: classifySurfaceKind(snapshot), Title: snapshot.WindowTitle,
		Generation: contextDigest, Screenshot: png, ScreenshotSHA256: hex.EncodeToString(screenshotDigest[:]),
		Actions: actions, ObservedAt: time.Now().UTC(),
	}
	d.mu.Lock()
	d.surfaces[id] = surfaceState{
		surface: surface, actions: allowed, consumedActions: consumedActions,
		replayTombstones: replayTombstones, sequence: snapshot.Sequence, identity: identity,
	}
	d.mu.Unlock()
	return surface, nil
}

func (d *Driver) prepareSurfaceSnapshot(ctx context.Context, snapshot UISnapshot) (UISnapshot, AndroidContainer, error) {
	if snapshot.PackageName != d.runtime.config.WeComPackage || snapshot.PackageName != DefaultWeComPackage {
		return snapshot, AndroidContainer{}, fmt.Errorf("%w: surface left the verified WeCom package", ErrTargetAmbiguous)
	}
	android, err := d.runtime.Android()
	if err != nil {
		return snapshot, AndroidContainer{}, err
	}
	snapshot = withForegroundActivity(ctx, android, snapshot)
	if err := validateSurfaceSnapshot(snapshot); err != nil {
		return snapshot, AndroidContainer{}, err
	}
	return snapshot, android, nil
}

func validateSurfaceSnapshot(snapshot UISnapshot) error {
	if snapshot.PackageName != DefaultWeComPackage {
		return fmt.Errorf("%w: surface is outside the official WeCom package", ErrTargetAmbiguous)
	}
	if snapshotRequiresUserAction(snapshot) || hasPrivacyConsentModalMarker(snapshot) {
		return fmt.Errorf("%w: sensitive verification or privacy screen is active", ErrUserActionRequired)
	}
	if authenticationSurface(snapshot) {
		return ErrAuthRequired
	}
	if surfaceHighRisk(snapshot) {
		return fmt.Errorf("%w: high-risk surface requires direct user interaction", ErrUserActionRequired)
	}
	if snapshot.Sequence <= 0 || snapshot.WindowID < 0 || strings.TrimSpace(snapshot.WindowClass) == "" {
		return fmt.Errorf("%w: surface window identity is incomplete", ErrStale)
	}
	return nil
}

func identityForSurface(snapshot UISnapshot) surfaceIdentity {
	return surfaceIdentity{
		packageName: snapshot.PackageName,
		windowID:    snapshot.WindowID,
		windowClass: strings.TrimSpace(snapshot.WindowClass),
	}
}

func (identity surfaceIdentity) valid() bool {
	return identity.packageName != "" && identity.windowID >= 0 && identity.windowClass != ""
}

func (identity surfaceIdentity) matches(snapshot UISnapshot) bool {
	return identity.valid() &&
		identity.packageName == snapshot.PackageName && identity.windowID == snapshot.WindowID &&
		identity.windowClass == strings.TrimSpace(snapshot.WindowClass)
}

func sameSurfaceObservation(before, after UISnapshot) bool {
	return before.Sequence > 0 && before.Sequence == after.Sequence &&
		identityForSurface(before).valid() && identityForSurface(before).matches(after)
}

func bindSurfaceAction(
	surfaceID string,
	snapshot UISnapshot,
	node Node,
	kind, label, risk, effect, contextDigest string,
) surfaceActionState {
	identity := identityForSurface(snapshot)
	nodeSignature := surfaceNodeSignature(node)
	replayID := "replay-" + digestID(strings.Join([]string{
		"wecom-surface-replay-v1", identity.packageName, strconv.Itoa(identity.windowID),
		identity.windowClass, node.ID, kind,
	}, "\x00"))
	payload := struct {
		Domain        string
		SurfaceID     string
		Sequence      int64
		PackageName   string
		WindowID      int
		WindowClass   string
		ContextDigest string
		NodeID        string
		NodeSignature string
		Label         string
		Bounds        Bounds
		Kind          string
		Risk          string
		Effect        string
	}{
		Domain: "wecom-surface-action-v1", SurfaceID: surfaceID, Sequence: snapshot.Sequence,
		PackageName: identity.packageName, WindowID: identity.windowID, WindowClass: identity.windowClass,
		ContextDigest: contextDigest, NodeID: node.ID, NodeSignature: nodeSignature,
		Label: label, Bounds: node.Bounds, Kind: kind, Risk: risk, Effect: effect,
	}
	encoded, _ := json.Marshal(payload)
	actionID := "act-" + digestID(string(encoded))
	disabled := risk == "high" || risk == "sensitive" || risk == "destructive" ||
		effect == "high_risk" || effect == "sensitive" || effect == "destructive"
	return surfaceActionState{
		advertised: core.Action{
			ID: actionID, TargetID: "target-" + digestID(replayID+"\x00"+nodeSignature),
			Label: label, Kind: kind, Risk: risk, Effect: effect, Disabled: disabled,
		},
		companion: CompanionAction{Kind: kind, NodeID: node.ID}, replayID: replayID,
		nodeSignature: nodeSignature, contextDigest: contextDigest, label: label,
		bounds: node.Bounds, sequence: snapshot.Sequence, identity: identity,
	}
}

func surfaceContextDigest(snapshot UISnapshot) string {
	payload := struct {
		Domain      string
		PackageName string
		WindowID    int
		WindowTitle string
		WindowClass string
		Nodes       []Node
	}{
		Domain: "wecom-surface-context-v1", PackageName: snapshot.PackageName,
		WindowID: snapshot.WindowID, WindowTitle: snapshot.WindowTitle,
		WindowClass: strings.TrimSpace(snapshot.WindowClass), Nodes: snapshot.Nodes,
	}
	encoded, _ := json.Marshal(payload)
	return digestID(string(encoded))
}

func surfaceNodeSignature(node Node) string {
	payload := struct {
		Domain string
		Node   Node
	}{Domain: "wecom-surface-node-v1", Node: node}
	encoded, _ := json.Marshal(payload)
	return digestID(string(encoded))
}

func validNodeBounds(bounds Bounds) bool {
	return bounds.Right > bounds.Left && bounds.Bottom > bounds.Top
}

func surfaceActionLabel(snapshot UISnapshot, node Node, kind string) string {
	labels := nodeUserFacingValues(node)
	if kind == ActionClick {
		// Android frequently puts a button's visible label on a child TextView.
		// Include direct, bounded children in the advertised label so an agent
		// never sees only a benign parent label while safety classification sees
		// a destructive child label.
		for _, candidate := range snapshot.Nodes {
			if candidate.ParentID != node.ID || !candidate.VisibleToUser ||
				!usableBounds(candidate.Bounds) || !boundsContains(node.Bounds, candidate.Bounds) {
				continue
			}
			labels = appendDistinctSemanticValues(labels, nodeUserFacingValues(candidate)...)
		}
	}
	label := strings.Join(labels, " — ")
	if label == "" {
		label = node.ClassName
	}
	switch kind {
	case ActionScrollForward:
		return "Scroll forward: " + label
	case ActionScrollBackward:
		return "Scroll backward: " + label
	default:
		return label
	}
}

func classifySurfaceAction(snapshot UISnapshot, node Node, kind string) (risk, effect string) {
	switch kind {
	case ActionScrollForward, ActionScrollBackward:
		return "low", "observe"
	case ActionSetText:
		if uniqueSearchInput(snapshot, node) {
			return "low", "search_input"
		}
		return "medium", "unknown"
	case ActionClick:
		if destructiveSurfaceAction(snapshot, node) ||
			(isWeComActivity(snapshot, weComConversationActivity) && sensitiveConversationControl(snapshot, node)) ||
			(!isWeComActivity(snapshot, weComConversationActivity) && sensitiveSurfaceLabel(actionSemanticEvidence(snapshot, node))) {
			return "high", "high_risk"
		}
		if externalWriteSurfaceAction(snapshot, node) {
			return "medium", "external_write"
		}
		if actionMatchesAny(snapshot, node,
			"返回", "关闭", "取消", "查看", "详情", "打开", "上一页", "下一页", "首页",
			"back", "close", "cancel", "view", "details", "open", "previous", "next", "home",
		) {
			return "low", "navigate"
		}
		return "medium", "unknown"
	default:
		return "medium", "unknown"
	}
}

func destructiveSurfaceAction(snapshot UISnapshot, target Node) bool {
	conversation := isWeComActivity(snapshot, weComConversationActivity)
	for _, node := range actionSemanticNodes(snapshot, target) {
		for _, value := range nodeUserFacingValues(node) {
			if (!conversation && destructiveSurfaceLabel(value, "")) ||
				(conversation && destructiveConversationControlLabel(value)) {
				return true
			}
		}
		if destructiveSurfaceLabel("", node.ViewID+" "+node.ClassName) {
			return true
		}
	}
	return false
}

func destructiveConversationControlLabel(value string) bool {
	if matchesAny(value,
		"删除", "移除", "清空数据", "清空记录", "清空历史", "清除数据", "擦除", "销毁",
		"注销", "永久注销", "格式化", "重置账号", "重置账户", "撤销授权", "取消授权",
		"delete", "remove", "erase", "clear all", "clear history", "clear data", "wipe",
		"destroy", "reset account", "revoke access", "revoke authorization", "format",
		"delete account", "delete message", "delete contact", "remove member",
	) {
		return true
	}
	folded := semanticFold(value)
	for _, prefix := range []string{
		"删除", "移除", "清空", "清除", "擦除", "销毁", "注销", "格式化", "重置", "撤销授权", "取消授权",
		"delete ", "remove ", "erase ", "clear ", "wipe ", "destroy ", "reset ", "revoke ", "format ",
	} {
		if strings.HasPrefix(folded, semanticFold(prefix)) && utf8.RuneCountInString(folded) <= 80 {
			return true
		}
	}
	return false
}

func externalWriteSurfaceAction(snapshot UISnapshot, target Node) bool {
	for _, node := range actionSemanticNodes(snapshot, target) {
		for _, value := range nodeUserFacingValues(node) {
			if externalWriteSurfaceLabel(value, "") {
				return true
			}
		}
		if externalWriteSurfaceLabel("", node.ViewID+" "+node.ClassName) {
			return true
		}
	}
	return false
}

func uniqueSearchInput(snapshot UISnapshot, target Node) bool {
	if !target.Editable || !target.Enabled || !target.VisibleToUser ||
		!containsAny(nodeLabel(target)+" "+target.ViewID, "搜索", "查找", "search", "find") {
		return false
	}
	matches := 0
	for _, node := range snapshot.Nodes {
		if node.Editable && node.Enabled && node.VisibleToUser &&
			containsAny(nodeLabel(node)+" "+node.ViewID, "搜索", "查找", "search", "find") {
			matches++
		}
	}
	return matches == 1
}

func externalWriteSurfaceLabel(label, viewID string) bool {
	return containsAny(
		label+" "+viewID,
		"发送", "发布", "提交", "保存", "确认", "点赞", "评论", "收藏", "关注", "分享", "转发",
		"报名", "加入", "申请", "send", "publish", "submit", "save", "confirm", "like", "comment",
		"favorite", "follow", "share", "forward", "join", "apply",
	)
}

func destructiveSurfaceLabel(label, viewID string) bool {
	return hasSemanticMarker(
		label+" "+viewID,
		"删除", "移除", "清空数据", "清空记录", "清空历史", "清除数据", "擦除",
		"抹掉数据", "销毁", "注销", "永久注销", "格式化", "重置账号", "重置账户",
		"撤销授权", "取消授权",
		"delete", "remove", "erase", "clear all", "clear history", "clear data", "wipe",
		"destroy", "reset account", "revoke access", "revoke authorization", "format",
	)
}

func navigationSurfaceLabel(label string) bool {
	return matchesAny(
		label,
		"返回", "关闭", "取消", "查看", "详情", "打开", "上一页", "下一页", "首页",
		"back", "close", "cancel", "view", "details", "open", "previous", "next", "home",
	)
}

func nodeSupportsSurfaceAction(node Node, kind string) bool {
	if node.ID == "" || !node.VisibleToUser || !node.Enabled || !validNodeBounds(node.Bounds) {
		return false
	}
	switch kind {
	case ActionSetText:
		return node.Editable
	case ActionClick:
		return node.Clickable
	case ActionScrollForward, ActionScrollBackward:
		return node.Scrollable
	default:
		return false
	}
}

func validateSurfaceActionState(surfaceID string, snapshot UISnapshot, expected surfaceActionState) error {
	if expected.sequence != snapshot.Sequence || !expected.identity.matches(snapshot) ||
		expected.contextDigest != surfaceContextDigest(snapshot) {
		return fmt.Errorf("%w: surface action context changed", ErrStale)
	}
	var matches []Node
	for _, node := range snapshot.Nodes {
		if node.ID == expected.companion.NodeID {
			matches = append(matches, node)
		}
	}
	if len(matches) > 1 {
		return fmt.Errorf("%w: surface action node identity is ambiguous", ErrTargetAmbiguous)
	}
	if len(matches) != 1 {
		return fmt.Errorf("%w: surface action node disappeared", ErrStale)
	}
	node := matches[0]
	if !nodeSupportsSurfaceAction(node, expected.companion.Kind) ||
		surfaceNodeSignature(node) != expected.nodeSignature || node.Bounds != expected.bounds {
		return fmt.Errorf("%w: surface action node semantics changed", ErrStale)
	}
	label := surfaceActionLabel(snapshot, node, expected.companion.Kind)
	risk, effect := classifySurfaceAction(snapshot, node, expected.companion.Kind)
	current := bindSurfaceAction(
		surfaceID, snapshot, node, expected.companion.Kind, label, risk, effect, expected.contextDigest,
	)
	if label != expected.label || current.advertised != expected.advertised ||
		current.replayID != expected.replayID || current.nodeSignature != expected.nodeSignature {
		return fmt.Errorf("%w: surface action identity changed", ErrStale)
	}
	return nil
}

func (d *Driver) consumeSurfaceAction(surfaceID, actionID string, expected surfaceActionState) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, exists := d.surfaces[surfaceID]
	if !exists {
		return false
	}
	current, exists := state.actions[actionID]
	if !exists || current.replayID != expected.replayID || current.nodeSignature != expected.nodeSignature ||
		current.contextDigest != expected.contextDigest || current.sequence != expected.sequence ||
		current.identity != expected.identity {
		return false
	}
	delete(state.actions, actionID)
	if state.consumedActions == nil {
		state.consumedActions = make(map[string]struct{})
	}
	state.consumedActions[actionID] = struct{}{}
	state.surface.Actions = withoutSurfaceActions(state.surface.Actions, map[string]struct{}{actionID: {}})
	d.surfaces[surfaceID] = state
	return true
}

func (d *Driver) tombstoneSurfaceReplay(surfaceID, replayID string) {
	if replayID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	state, exists := d.surfaces[surfaceID]
	if !exists {
		return
	}
	if state.replayTombstones == nil {
		state.replayTombstones = make(map[string]struct{})
	}
	state.replayTombstones[replayID] = struct{}{}
	removed := make(map[string]struct{})
	for actionID, action := range state.actions {
		if action.replayID == replayID {
			delete(state.actions, actionID)
			removed[actionID] = struct{}{}
		}
	}
	state.surface.Actions = withoutSurfaceActions(state.surface.Actions, removed)
	d.surfaces[surfaceID] = state
}

func withoutSurfaceActions(actions []core.Action, removed map[string]struct{}) []core.Action {
	if len(removed) == 0 {
		return actions
	}
	result := make([]core.Action, 0, len(actions))
	for _, action := range actions {
		if _, drop := removed[action.ID]; !drop {
			result = append(result, action)
		}
	}
	return result
}

func shouldTombstoneSurfaceReplay(effect string, succeeded bool) bool {
	switch strings.ToLower(strings.TrimSpace(effect)) {
	case "observe", "search_input":
		return false
	case "navigate":
		return !succeeded
	default:
		return true
	}
}

func privacyConsentActions(snapshot UISnapshot, screenshotPNG []byte, accountID string) []core.AuthAction {
	if snapshotRequiresUserAction(snapshot) {
		return nil
	}
	if _, err := uniquePrivacyConsentTarget(snapshot); err != nil {
		return nil
	}
	if len(screenshotPNG) == 0 {
		return nil
	}
	return []core.AuthAction{{
		ID:                   bindAuthActionID(acceptPrivacyPolicyAction, snapshot, screenshotPNG, accountID),
		ReplayKey:            acceptPrivacyPolicyAction,
		Label:                "同意企业微信隐私政策并继续",
		Risk:                 "high",
		Confirmation:         "请确认你已阅读官方企业微信客户端中显示的隐私政策，并同意后继续。",
		RequiresConfirmation: true,
		ImageBound:           true,
	}}
}

type weComLoginTargets struct {
	terms  Node
	wechat Node
	email  Node
	phone  Node
}

func authenticationActions(snapshot UISnapshot, screenshotPNG []byte, accountID string) ([]core.AuthAction, string) {
	if snapshotRequiresUserAction(snapshot) {
		return nil, ""
	}
	if actions := privacyConsentActions(snapshot, screenshotPNG, accountID); len(actions) != 0 {
		return actions, "Review and accept the official WeCom privacy policy to continue"
	}
	// Never fall through to controls behind a complete or partially observed
	// first-run privacy modal.
	if hasPrivacyConsentModalMarker(snapshot) {
		return nil, ""
	}
	targets, err := weComLoginMethodTargets(snapshot)
	if err != nil || len(screenshotPNG) == 0 {
		return nil, ""
	}
	if !weComLoginTermsAccepted(targets.terms) {
		return []core.AuthAction{{
			ID:                   bindAuthActionID(acceptWeComLoginTermsAction, snapshot, screenshotPNG, accountID),
			ReplayKey:            acceptWeComLoginTermsAction,
			Label:                "阅读并同意企业微信登录协议",
			Risk:                 "high",
			Confirmation:         "请确认你已阅读官方企业微信客户端中显示的软件许可及服务协议与隐私政策，并同意后继续。",
			RequiresConfirmation: true,
			ImageBound:           true,
		}}, "Review and accept the official WeCom login agreements to continue"
	}
	return []core.AuthAction{{
		ID:                   bindAuthActionID(continueWeComWithWechatAction, snapshot, screenshotPNG, accountID),
		ReplayKey:            continueWeComWithWechatAction,
		Label:                "使用微信继续登录企业微信",
		Risk:                 "high",
		Confirmation:         "请确认使用当前微信身份继续登录企业微信；后续扫码、手机确认或验证码仍由你本人完成。",
		RequiresConfirmation: true,
		ImageBound:           true,
	}}, "Confirm that you want to continue with the current WeChat identity"
}

func bindAuthActionID(operation string, snapshot UISnapshot, screenshotPNG []byte, accountID string) string {
	encoded := encodeAuthGenerationFrame(snapshot, accountID)
	if len(encoded) == 0 || len(screenshotPNG) == 0 {
		return ""
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("wechatcopilot/wecom/auth-action/v1\x00"))
	_, _ = digest.Write(encoded)
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(screenshotPNG)
	return operation + "." + hex.EncodeToString(digest.Sum(nil))
}

func encodeAuthGenerationFrame(snapshot UISnapshot, accountID string) []byte {
	frame := struct {
		AccountID   string `json:"account_id"`
		Sequence    int64  `json:"sequence"`
		PackageName string `json:"package_name"`
		WindowID    int    `json:"window_id"`
		WindowTitle string `json:"window_title"`
		WindowClass string `json:"window_class"`
		Nodes       []Node `json:"nodes"`
	}{
		AccountID: accountID, Sequence: snapshot.Sequence, PackageName: snapshot.PackageName,
		WindowID: snapshot.WindowID, WindowTitle: snapshot.WindowTitle,
		WindowClass: strings.TrimSpace(snapshot.WindowClass), Nodes: snapshot.Nodes,
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return nil
	}
	return encoded
}

func sameAuthenticationObservation(before, after UISnapshot) bool {
	if !sameSurfaceObservation(before, after) {
		return false
	}
	beforeFrame := encodeAuthGenerationFrame(before, "")
	afterFrame := encodeAuthGenerationFrame(after, "")
	if len(beforeFrame) == 0 || len(afterFrame) == 0 {
		return false
	}
	beforeDigest := sha256.Sum256(beforeFrame)
	afterDigest := sha256.Sum256(afterFrame)
	return beforeDigest == afterDigest
}

func parseAuthActionID(actionID string) (string, bool) {
	for _, operation := range []string{
		acceptPrivacyPolicyAction,
		acceptWeComLoginTermsAction,
		continueWeComWithWechatAction,
	} {
		prefix := operation + "."
		if !strings.HasPrefix(actionID, prefix) {
			continue
		}
		generation := strings.TrimPrefix(actionID, prefix)
		if len(generation) != authActionGenerationHexLength || strings.ToLower(generation) != generation {
			return "", false
		}
		decoded, err := hex.DecodeString(generation)
		return operation, err == nil && len(decoded) == sha256.Size
	}
	return "", false
}

func weComLoginMethodTargets(snapshot UISnapshot) (weComLoginTargets, error) {
	if snapshot.Sequence <= 0 || !isWeComActivity(snapshot, weComLoginWxAuthActivity) ||
		snapshotRequiresUserAction(snapshot) || hasPrivacyConsentModalMarker(snapshot) {
		return weComLoginTargets{}, fmt.Errorf("%w: official WeCom login method screen is not active", ErrStale)
	}
	visible := visibleSnapshotText(snapshot)
	if !containsAny(visible, "read and agree") ||
		!containsAny(visible, "software licensing and service agreements") ||
		!containsAny(visible, "privacy policy") {
		return weComLoginTargets{}, fmt.Errorf("%w: official WeCom login agreement markers are incomplete", ErrStale)
	}

	wechat, err := uniqueVisibleNormalizedLabelTarget(snapshot, "Continue with WeChat")
	if err != nil {
		return weComLoginTargets{}, fmt.Errorf("WeChat login method target: %w", err)
	}
	email, err := uniqueVisibleNormalizedLabelTarget(snapshot, "Continue with Email")
	if err != nil {
		return weComLoginTargets{}, fmt.Errorf("email login method target: %w", err)
	}
	phone, err := uniqueVisibleNormalizedLabelTarget(snapshot, "Continue with Phone")
	if err != nil {
		return weComLoginTargets{}, fmt.Errorf("phone login method target: %w", err)
	}
	if wechat.ID == email.ID || wechat.ID == phone.ID || email.ID == phone.ID {
		return weComLoginTargets{}, fmt.Errorf("%w: WeCom login methods share one clickable target", ErrTargetAmbiguous)
	}

	agreementControls := matchingNodes(snapshot, isWeComLoginAgreementImage)
	if len(agreementControls) == 0 {
		return weComLoginTargets{}, fmt.Errorf("%w: WeCom login agreement control is unavailable", ErrStale)
	}
	if len(agreementControls) != 1 {
		return weComLoginTargets{}, fmt.Errorf("%w: multiple WeCom login agreement controls", ErrTargetAmbiguous)
	}
	terms := agreementControls[0]
	if !isWeComLoginTermsControl(terms) {
		return weComLoginTargets{}, fmt.Errorf("%w: WeCom login agreement state is unavailable or incompatible", ErrStale)
	}
	if terms.ID == "" || !terms.VisibleToUser || !terms.Enabled || !terms.Clickable || !usableBounds(terms.Bounds) {
		return weComLoginTargets{}, fmt.Errorf("%w: WeCom login agreement control is not safely actionable", ErrStale)
	}
	if terms.ID == wechat.ID || terms.ID == email.ID || terms.ID == phone.ID {
		return weComLoginTargets{}, fmt.Errorf("%w: WeCom agreement and login method share one target", ErrTargetAmbiguous)
	}
	return weComLoginTargets{terms: terms, wechat: wechat, email: email, phone: phone}, nil
}

func isWeComLoginTermsControl(node Node) bool {
	return isWeComLoginAgreementImage(node) &&
		!node.Checkable &&
		!node.Checked &&
		node.Selected != nil
}

func isWeComLoginAgreementImage(node Node) bool {
	return node.ClassName == androidImageViewClass && node.ViewID == weComLoginAgreementViewID
}

func weComLoginTermsAccepted(node Node) bool {
	return isWeComLoginAgreementImage(node) && node.Selected != nil && *node.Selected
}

func uniqueVisibleNormalizedLabelTarget(snapshot UISnapshot, label string) (Node, error) {
	byID := make(map[string]Node, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		byID[node.ID] = node
	}
	want := normalizedVisibleLabel(label)
	candidates := make(map[string]Node)
	for _, node := range snapshot.Nodes {
		if node.ID == "" || !node.VisibleToUser || normalizedVisibleLabel(nodeLabel(node)) != want {
			continue
		}
		current := node
		for depth := 0; depth <= len(snapshot.Nodes); depth++ {
			if current.Enabled && current.Clickable && current.VisibleToUser && usableBounds(current.Bounds) {
				candidates[current.ID] = current
				break
			}
			if current.ParentID == "" {
				break
			}
			parent, ok := byID[current.ParentID]
			if !ok || parent.ID == current.ID {
				break
			}
			current = parent
		}
	}
	if len(candidates) == 0 {
		return Node{}, fmt.Errorf("%w: matching visible clickable target is unavailable", ErrStale)
	}
	if len(candidates) != 1 {
		return Node{}, fmt.Errorf("%w: multiple matching visible clickable targets", ErrTargetAmbiguous)
	}
	for _, candidate := range candidates {
		return candidate, nil
	}
	return Node{}, fmt.Errorf("%w: matching visible clickable target is unavailable", ErrStale)
}

func normalizedVisibleLabel(value string) string {
	return semanticFold(value)
}

func hasPrivacyConsentModalMarker(snapshot UISnapshot) bool {
	if snapshot.PackageName != DefaultWeComPackage || isWeComActivity(snapshot, weComConversationActivity) {
		return false
	}
	visible := visibleSnapshotText(snapshot)
	hasPolicy := containsAny(visible, "privacy policy", "隐私政策", "隐私保护指引")
	hasWelcome := containsAny(visible, "welcome to wecom", "欢迎使用企业微信")
	if !hasPolicy || !hasWelcome {
		return false
	}
	_, agreeErr := uniqueVisibleLabelTarget(snapshot, "Agree", "同意")
	_, disagreeErr := uniqueVisibleLabelTarget(snapshot, "Disagree", "不同意")
	if agreeErr == nil && disagreeErr == nil {
		return true
	}
	// On an exact first-run Activity, incomplete policy controls still mean the
	// modal must fail closed rather than leak into generic surface handling.
	return isWeComActivity(snapshot, weComLoginWxAuthActivity) || isWeComActivity(snapshot, weComLaunchActivity)
}

func hasWeComLoginMethodMarker(snapshot UISnapshot) bool {
	if snapshot.PackageName != DefaultWeComPackage || isWeComActivity(snapshot, weComConversationActivity) {
		return false
	}
	visible := visibleSnapshotText(snapshot)
	if !containsAny(visible, "read and agree") ||
		!containsAny(visible, "software licensing and service agreements") ||
		!containsAny(visible, "privacy policy") {
		return false
	}
	labels := make(map[string]bool)
	for _, node := range snapshot.Nodes {
		if !node.VisibleToUser {
			continue
		}
		switch normalizedVisibleLabel(nodeLabel(node)) {
		case "continue with wechat", "continue with email", "continue with phone":
			labels[normalizedVisibleLabel(nodeLabel(node))] = true
		}
	}
	return len(labels) == 3 && hasWeComLoginAgreementControl(snapshot)
}

func uniquePrivacyConsentTarget(snapshot UISnapshot) (Node, error) {
	if !privacyConsentPage(snapshot) || snapshot.Sequence <= 0 {
		return Node{}, fmt.Errorf("%w: official privacy consent screen is not active", ErrStale)
	}
	agree, err := uniqueVisibleLabelTarget(snapshot, "Agree", "同意")
	if err != nil {
		return Node{}, fmt.Errorf("privacy consent agree target: %w", err)
	}
	disagree, err := uniqueVisibleLabelTarget(snapshot, "Disagree", "不同意")
	if err != nil {
		return Node{}, fmt.Errorf("privacy consent disagree target: %w", err)
	}
	if agree.ID == disagree.ID {
		return Node{}, fmt.Errorf("%w: privacy consent choices share one clickable target", ErrTargetAmbiguous)
	}
	return agree, nil
}

func uniqueVisibleLabelTarget(snapshot UISnapshot, labels ...string) (Node, error) {
	byID := make(map[string]Node, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		byID[node.ID] = node
	}
	candidates := make(map[string]Node)
	for _, node := range snapshot.Nodes {
		if node.ID == "" || !node.VisibleToUser || !matchesAny(nodeLabel(node), labels...) {
			continue
		}
		current := node
		for depth := 0; depth <= len(snapshot.Nodes); depth++ {
			if current.Enabled && current.Clickable && current.VisibleToUser && usableBounds(current.Bounds) {
				candidates[current.ID] = current
				break
			}
			if current.ParentID == "" {
				break
			}
			parent, ok := byID[current.ParentID]
			if !ok || parent.ID == current.ID {
				break
			}
			current = parent
		}
	}
	if len(candidates) == 0 {
		return Node{}, fmt.Errorf("%w: matching visible clickable target is unavailable", ErrStale)
	}
	if len(candidates) != 1 {
		return Node{}, fmt.Errorf("%w: multiple matching visible clickable targets", ErrTargetAmbiguous)
	}
	for _, candidate := range candidates {
		return candidate, nil
	}
	return Node{}, fmt.Errorf("%w: matching visible clickable target is unavailable", ErrStale)
}

func privacyConsentPage(snapshot UISnapshot) bool {
	if snapshot.PackageName != DefaultWeComPackage ||
		(!isWeComActivity(snapshot, weComLoginWxAuthActivity) && !isWeComActivity(snapshot, weComLaunchActivity)) {
		return false
	}
	return privacyConsentMarkers(snapshot)
}

func privacyConsentMarkers(snapshot UISnapshot) bool {
	text := visibleSnapshotText(snapshot)
	english := containsAny(text, "privacy policy") && containsAny(text, "welcome to wecom")
	chinese := containsAny(text, "隐私政策") && containsAny(text, "欢迎使用企业微信")
	return english || chinese
}

func visibleSnapshotText(snapshot UISnapshot) string {
	var builder strings.Builder
	builder.WriteString(snapshot.WindowTitle)
	for _, node := range snapshot.Nodes {
		if !node.VisibleToUser {
			continue
		}
		builder.WriteByte('\n')
		builder.WriteString(node.Text)
		builder.WriteByte(' ')
		builder.WriteString(node.ContentDescription)
	}
	return strings.ToLower(builder.String())
}

func waitForPrivacyConsentDismissal(ctx context.Context, client *CompanionClient, android AndroidContainer, previous int64) error {
	deadline := time.Now().Add(10 * time.Second)
	changed := false
	dismissedObservations := 0
	for {
		snapshot, err := client.Snapshot(ctx)
		if err == nil {
			if snapshot.Sequence > previous {
				changed = true
			}
			if changed {
				snapshot = withForegroundActivity(ctx, android, snapshot)
				activity := strings.TrimSpace(snapshot.WindowClass)
				if snapshot.PackageName == DefaultWeComPackage && strings.HasPrefix(activity, DefaultWeComPackage+".") {
					if snapshotRequiresUserAction(snapshot) {
						return ErrUserActionRequired
					}
					if privacyConsentMarkers(snapshot) {
						dismissedObservations = 0
					} else {
						dismissedObservations++
						if dismissedObservations >= 2 {
							return nil
						}
					}
				} else {
					dismissedObservations = 0
				}
			}
		} else {
			dismissedObservations = 0
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: privacy consent screen did not close", ErrStale)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func waitForWeComLoginTermsAccepted(ctx context.Context, client *CompanionClient, android AndroidContainer, previous int64) error {
	deadline := time.Now().Add(10 * time.Second)
	changed := false
	acceptedObservations := 0
	for {
		snapshot, err := client.Snapshot(ctx)
		if err == nil {
			if snapshot.Sequence > previous {
				changed = true
			}
			if changed {
				snapshot = withForegroundActivity(ctx, android, snapshot)
				if snapshotHasHardAuthRisk(snapshot) {
					return ErrUserActionRequired
				}
				targets, targetErr := weComLoginMethodTargets(snapshot)
				if targetErr == nil && weComLoginTermsAccepted(targets.terms) {
					acceptedObservations++
					if acceptedObservations >= 2 {
						return nil
					}
				} else {
					acceptedObservations = 0
				}
			}
		} else {
			acceptedObservations = 0
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: WeCom login agreement control did not become stably selected", ErrStale)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func waitForWeComLoginMethodDismissal(ctx context.Context, client *CompanionClient, android AndroidContainer, previous int64) error {
	deadline := time.Now().Add(10 * time.Second)
	changed := false
	dismissedObservations := 0
	for {
		snapshot, err := client.Snapshot(ctx)
		if err == nil {
			if snapshot.Sequence > previous {
				changed = true
			}
			if changed {
				snapshot = withForegroundActivity(ctx, android, snapshot)
				if snapshotHasHardAuthRisk(snapshot) {
					return ErrUserActionRequired
				}
				activity := strings.TrimSpace(snapshot.WindowClass)
				if snapshot.PackageName == DefaultWeComPackage &&
					strings.HasPrefix(activity, DefaultWeComPackage+".") &&
					!hasWeComLoginMethodMarker(snapshot) {
					dismissedObservations++
					if dismissedObservations >= 2 {
						return nil
					}
				} else {
					dismissedObservations = 0
				}
			}
		} else {
			dismissedObservations = 0
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: WeCom login method screen did not close safely", ErrStale)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func usableBounds(bounds Bounds) bool {
	return bounds.Right > bounds.Left && bounds.Bottom > bounds.Top && bounds.Right > 0 && bounds.Bottom > 0
}

func classifyLogin(snapshot UISnapshot) (core.RuntimeState, string) {
	// Security and device-verification challenges take precedence over stale
	// navigation nodes retained behind an overlay or Activity transition.
	if snapshotRequiresUserAction(snapshot) || structuredSecurityChallenge(snapshot) {
		return core.StateAuthRequired, "direct user action is required for the official WeCom security challenge"
	}
	if snapshot.PackageName == DefaultWeComPackage {
		if isWeComActivity(snapshot, weComLoginWxAuthActivity) ||
			isWeComActivity(snapshot, weComSMSVerifyActivity) ||
			hasWeComLoginAgreementControl(snapshot) || hasWeComLoginMethodMarker(snapshot) ||
			hasPrivacyConsentModalMarker(snapshot) {
			return core.StateAuthRequired, "complete login in the official WeCom client"
		}
		if isWeComActivity(snapshot, weComLaunchActivity) {
			return core.StateStarting, "waiting for the official WeCom login window"
		}
	}
	bottomNav := 0
	for _, label := range []string{"消息", "邮件", "文档", "工作台", "通讯录"} {
		for _, node := range snapshot.Nodes {
			if node.VisibleToUser && matchesAny(nodeLabel(node), label) {
				bottomNav++
				break
			}
		}
	}
	if snapshot.PackageName == DefaultWeComPackage && bottomNav >= 2 {
		return core.StateOnline, ""
	}
	if snapshot.PackageName == "" {
		return core.StateStarting, "waiting for an accessibility window"
	}
	if snapshot.PackageName == DefaultWeComPackage && strings.TrimSpace(snapshot.WindowClass) == "" {
		return core.StateDegraded, "official foreground activity is unavailable for safe classification"
	}
	return core.StateDegraded, "official client state is not recognized by this compatibility profile"
}

func isWeComActivity(snapshot UISnapshot, activity string) bool {
	return snapshot.PackageName == DefaultWeComPackage &&
		strings.TrimSpace(snapshot.WindowClass) == activity
}

func classifySurfaceKind(snapshot UISnapshot) string {
	classText := strings.ToLower(snapshot.WindowTitle + " " + snapshot.WindowClass + " " + snapshot.PackageName + " " + snapshotText(snapshot))
	if strings.Contains(classText, "小程序") || strings.Contains(classText, "miniprogram") {
		return "miniprogram"
	}
	if strings.Contains(classText, "http://") || strings.Contains(classText, "https://") || strings.Contains(classText, "webview") {
		return "web"
	}
	return "android_surface"
}

func snapshotText(snapshot UISnapshot) string {
	var builder strings.Builder
	builder.WriteString(snapshot.WindowTitle)
	for _, node := range snapshot.Nodes {
		builder.WriteByte('\n')
		builder.WriteString(node.Text)
		builder.WriteByte(' ')
		builder.WriteString(node.ContentDescription)
	}
	return strings.ToLower(builder.String())
}

func countEditable(snapshot UISnapshot) int { return len(editableNodes(snapshot)) }

func editableNodes(snapshot UISnapshot) []Node {
	return matchingNodes(snapshot, func(node Node) bool { return node.Editable && node.Enabled })
}

func validSMSAuthSnapshot(snapshot UISnapshot) bool {
	if snapshot.PackageName != DefaultWeComPackage ||
		!isWeComActivity(snapshot, weComSMSVerifyActivity) ||
		snapshot.Sequence <= 0 || snapshotRequiresUserAction(snapshot) {
		return false
	}
	state, _ := classifyLogin(snapshot)
	if state != core.StateAuthRequired || !containsAny(visibleSnapshotText(snapshot), "验证码", "verification code", "短信") {
		return false
	}
	editable := editableNodes(snapshot)
	return len(editable) == 1 && editable[0].VisibleToUser && usableBounds(editable[0].Bounds)
}

func uniqueNode(snapshot UISnapshot, match func(Node) bool) (Node, error) {
	nodes := matchingNodes(snapshot, match)
	if len(nodes) == 0 {
		return Node{}, ErrNotFound
	}
	if len(nodes) > 1 {
		return Node{}, ErrTargetAmbiguous
	}
	return nodes[0], nil
}

func matchingNodes(snapshot UISnapshot, match func(Node) bool) []Node {
	result := make([]Node, 0)
	for _, node := range snapshot.Nodes {
		if match(node) {
			result = append(result, node)
		}
	}
	return result
}

func nodeLabel(node Node) string {
	if node.Text != "" {
		return strings.TrimSpace(node.Text)
	}
	return strings.TrimSpace(node.ContentDescription)
}

// nodeSemanticEvidence is deliberately risk-only: exact interaction matching
// may use the primary visible label, but safety decisions must never let Text
// hide a conflicting ContentDescription or resource identifier.
func nodeSemanticEvidence(node Node) string {
	return strings.Join(nodeSemanticValues(node), " ")
}

func nodeSemanticValues(node Node) []string {
	values := nodeUserFacingValues(node)
	return appendDistinctSemanticValues(values, node.ViewID, node.ClassName)
}

func nodeUserFacingValues(node Node) []string {
	return appendDistinctSemanticValues(nil, node.Text, node.ContentDescription)
}

func appendDistinctSemanticValues(values []string, candidates ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(candidates))
	for _, value := range values {
		seen[semanticFold(value)] = struct{}{}
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		key := semanticFold(candidate)
		if candidate == "" || key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, candidate)
	}
	return values
}

// actionSemanticNodes returns only nodes structurally and geometrically bound
// to the target. This catches labels placed on nested TextViews without
// treating unrelated page or chat copy as evidence for the action.
func actionSemanticNodes(snapshot UISnapshot, target Node) []Node {
	result := []Node{target}
	byID := make(map[string]Node, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if node.ID != "" {
			byID[node.ID] = node
		}
	}
	added := map[string]struct{}{target.ID: {}}
	for _, candidate := range snapshot.Nodes {
		if candidate.ID == "" || candidate.ID == target.ID || !candidate.VisibleToUser ||
			!usableBounds(candidate.Bounds) || !boundsContains(target.Bounds, candidate.Bounds) ||
			!nodeDescendsFrom(candidate, target.ID, byID) {
			continue
		}
		if _, exists := added[candidate.ID]; exists {
			continue
		}
		added[candidate.ID] = struct{}{}
		result = append(result, candidate)
	}
	// Some Android layouts place the semantic description on an equal-bounds
	// wrapper. Include at most the exact-bounds ancestor chain; never widen to a
	// page container.
	current := target
	for depth := 0; depth < 2 && current.ParentID != ""; depth++ {
		parent, exists := byID[current.ParentID]
		if !exists || parent.ID == current.ID || !parent.VisibleToUser || parent.Bounds != target.Bounds {
			break
		}
		if _, exists := added[parent.ID]; !exists {
			added[parent.ID] = struct{}{}
			result = append(result, parent)
		}
		current = parent
	}
	return result
}

func nodeDescendsFrom(node Node, ancestorID string, byID map[string]Node) bool {
	current := node
	for depth := 0; depth <= len(byID) && current.ParentID != ""; depth++ {
		if current.ParentID == ancestorID {
			return true
		}
		parent, exists := byID[current.ParentID]
		if !exists || parent.ID == current.ID {
			return false
		}
		current = parent
	}
	return false
}

func actionSemanticEvidence(snapshot UISnapshot, target Node) string {
	values := make([]string, 0)
	for _, node := range actionSemanticNodes(snapshot, target) {
		values = appendDistinctSemanticValues(values, nodeSemanticValues(node)...)
	}
	return strings.Join(values, " ")
}

func actionMatchesAny(snapshot UISnapshot, target Node, candidates ...string) bool {
	for _, node := range actionSemanticNodes(snapshot, target) {
		for _, value := range nodeSemanticValues(node) {
			if matchesAny(value, candidates...) {
				return true
			}
		}
	}
	return false
}

func containsAny(value string, candidates ...string) bool {
	value = semanticFold(value)
	for _, candidate := range candidates {
		if strings.Contains(value, semanticFold(candidate)) {
			return true
		}
	}
	return false
}

func matchesAny(value string, candidates ...string) bool {
	value = semanticFold(value)
	for _, candidate := range candidates {
		if value == semanticFold(candidate) {
			return true
		}
	}
	return false
}

func semanticFold(value string) string {
	folded := strings.Map(func(character rune) rune {
		if defaultIgnorableRune(character) {
			return -1
		}
		if character >= 0xff01 && character <= 0xff5e {
			character -= 0xfee0
		}
		if character == 0x3000 {
			return ' '
		}
		return unicode.ToLower(character)
	}, value)
	folded = strings.Join(strings.Fields(folded), " ")
	runes := []rune(folded)
	result := make([]rune, 0, len(runes))
	for index, character := range runes {
		if character == ' ' && index > 0 && index+1 < len(runes) &&
			unicode.Is(unicode.Han, runes[index-1]) && unicode.Is(unicode.Han, runes[index+1]) {
			continue
		}
		result = append(result, character)
	}
	return string(result)
}

func defaultIgnorableRune(character rune) bool {
	if unicode.Is(unicode.Cc, character) || unicode.Is(unicode.Cf, character) || unicode.Is(unicode.Cs, character) {
		return true
	}
	return character == 0x034f ||
		character >= 0x115f && character <= 0x1160 ||
		character >= 0x17b4 && character <= 0x17b5 ||
		character >= 0x180b && character <= 0x180f ||
		character == 0x3164 ||
		character >= 0xfe00 && character <= 0xfe0f ||
		character == 0xffa0 ||
		character >= 0xfff0 && character <= 0xfff8 ||
		character >= 0xe0100 && character <= 0xe01ef
}

func hasSemanticMarker(value string, candidates ...string) bool {
	valueRunes := []rune(semanticFold(value))
	for _, candidate := range candidates {
		markerRunes := []rune(semanticFold(candidate))
		if len(markerRunes) == 0 || len(markerRunes) > len(valueRunes) {
			continue
		}
		asciiMarker := true
		for _, character := range markerRunes {
			if character > unicode.MaxASCII {
				asciiMarker = false
				break
			}
		}
		for start := 0; start+len(markerRunes) <= len(valueRunes); start++ {
			if string(valueRunes[start:start+len(markerRunes)]) != string(markerRunes) {
				continue
			}
			end := start + len(markerRunes)
			beforeBoundary := start == 0 ||
				(!unicode.IsLetter(valueRunes[start-1]) && !unicode.IsDigit(valueRunes[start-1]))
			afterBoundary := end == len(valueRunes) ||
				(!unicode.IsLetter(valueRunes[end]) && !unicode.IsDigit(valueRunes[end]))
			if !asciiMarker || beforeBoundary && afterBoundary {
				return true
			}
		}
	}
	return false
}

func waitForSnapshotChange(ctx context.Context, client *CompanionClient, previous int64) (UISnapshot, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		snapshot, err := client.Snapshot(ctx)
		if err == nil && snapshot.Sequence > previous {
			return snapshot, nil
		}
		if time.Now().After(deadline) {
			return UISnapshot{}, fmt.Errorf("%w: timed out waiting for official client UI change", ErrStale)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return UISnapshot{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func waitForDirectionalOutgoingBubble(
	ctx context.Context,
	client *CompanionClient,
	android AndroidContainer,
	title string,
	binding chatBinding,
	text string,
	baseline int,
	previousSequence int64,
	timeout time.Duration,
) (bool, error) {
	deadline := time.Now().Add(timeout)
	stableEvidence := ""
	stableObservations := 0
	for {
		snapshot, err := client.Snapshot(ctx)
		if err != nil {
			return false, err
		}
		snapshot = withForegroundActivity(ctx, android, snapshot)
		frame, frameErr := validateChatFrame(snapshot, title, &binding, false)
		if frameErr != nil {
			return false, frameErr
		}
		evidence := outgoingBubbleEvidence(snapshot, text, binding)
		if snapshot.Sequence > previousSequence && frame.composer.Text != text && len(evidence) == baseline+1 {
			key := strings.Join(evidence, "\n")
			if key == stableEvidence {
				stableObservations++
			} else {
				stableEvidence = key
				stableObservations = 1
			}
			if stableObservations >= 2 {
				return true, nil
			}
		} else {
			stableEvidence = ""
			stableObservations = 0
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

type eventHistory struct {
	Events   []CompanionEvent
	Complete bool
}

func allEvents(ctx context.Context, client *CompanionClient) (eventHistory, error) {
	const maxEvents = 2_000
	result := eventHistory{Events: make([]CompanionEvent, 0, 500), Complete: true}
	var cursor int64
	for {
		limit := 500
		if remaining := maxEvents - len(result.Events); remaining < limit {
			limit = remaining
		}
		if limit == 0 {
			// One bounded look-ahead distinguishes an exactly full journal from
			// a result truncated by this driver's safety cap.
			page, err := client.Events(ctx, cursor, 1)
			if err != nil {
				return eventHistory{}, err
			}
			if !page.Complete || len(page.Events) != 0 {
				result.Complete = false
			}
			return result, nil
		}
		page, err := client.Events(ctx, cursor, limit)
		if err != nil {
			return eventHistory{}, err
		}
		if !page.Complete {
			result.Complete = false
		}
		if len(page.Events) == 0 {
			return result, nil
		}
		result.Events = append(result.Events, page.Events...)
		if page.NextCursor <= cursor {
			result.Complete = false
			return result, nil
		}
		cursor = page.NextCursor
	}
}

func snapshotConfirmsTitle(snapshot UISnapshot, title string) bool {
	if title == "" {
		return false
	}
	if snapshot.WindowTitle == title {
		return true
	}
	for _, node := range snapshot.Nodes {
		if nodeLabel(node) == title {
			return true
		}
	}
	return false
}

func sensitiveSurfaceLabel(label string) bool {
	return containsAny(
		label,
		"支付", "付款", "收款", "转账", "红包", "授权", "允许", "身份验证", "实名认证",
		"账号安全", "账户安全", "购买", "下单", "充值", "提现", "签署", "登录", "密码", "银行卡",
		"pay", "payment", "transfer", "red packet", "authorize", "permission", "identity verification",
		"account security", "purchase", "checkout", "place order", "top up", "recharge", "withdraw",
		"sign agreement", "sign in", "log in", "login", "password", "bank card",
	)
}

func surfaceHighRisk(snapshot UISnapshot) bool { return structuredHighRiskSurface(snapshot) }

func authenticationSurface(snapshot UISnapshot) bool {
	if snapshot.PackageName != DefaultWeComPackage {
		return false
	}
	if isWeComActivity(snapshot, weComLoginWxAuthActivity) ||
		isWeComActivity(snapshot, weComSMSVerifyActivity) ||
		hasWeComLoginAgreementControl(snapshot) || hasWeComLoginMethodMarker(snapshot) ||
		hasPrivacyConsentModalMarker(snapshot) {
		return true
	}
	return structuredSMSFallback(snapshot) || structuredQRFallback(snapshot)
}

func hasWeComLoginAgreementControl(snapshot UISnapshot) bool {
	for _, node := range snapshot.Nodes {
		if isWeComLoginAgreementImage(node) {
			return true
		}
	}
	return false
}

func snapshotRequiresUserAction(snapshot UISnapshot) bool {
	return structuredSecurityChallenge(snapshot)
}

func snapshotHasHardAuthRisk(snapshot UISnapshot) bool {
	return structuredSecurityChallenge(snapshot)
}

func conversationID(accountID, conversationKey string) string {
	return "wecom-conv-" + digestID(accountID+"\x00"+strings.ToLower(conversationKey))
}

func messageID(accountID string, sequence int64) string {
	return "wecom-msg-" + digestID(accountID+":"+strconv.FormatInt(sequence, 10))
}

func notificationSurfaceReference(accountID string, sequence int64) string {
	sequenceText := strconv.FormatInt(sequence, 10)
	return "wecom-notification:" + sequenceText + ":" + notificationSurfaceScope(accountID, sequence)
}

func notificationSurfaceScope(accountID string, sequence int64) string {
	return digestID("wecom-notification\x00" + accountID + "\x00" + strconv.FormatInt(sequence, 10))
}

func digestID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}
