package auth

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/driver"
	qrcode "github.com/skip2/go-qrcode"
)

const (
	challengeTTL       = 10 * time.Minute
	completedRetention = 60 * time.Second
	// The official clients can briefly construct their main window before a
	// server-side login attempt is finally accepted. A single ONLINE probe is
	// therefore only a candidate: keep the challenge open until the same
	// runtime remains online across several fresh observations and this entire
	// stability window. This prevents the browser and `accounts login --wait`
	// from reporting success immediately before the client falls back to its
	// login screen.
	onlineStabilityWindow           = 15 * time.Second
	minimumStableOnlineObservations = 3
	// A confirmed generation is preconsumed before driver invocation. The
	// challenge also has a total cap for distinct logical onboarding stages.
	// A user-confirmed action may be offered again only when the driver
	// definitively reports that it rejected the request before dispatch. This
	// bounded allowance lets a transient generation/focus revalidation recover
	// without ever replaying an accepted or uncertain operation.
	maxAuthActionAttemptsPerAction    = 3
	maxAuthActionAttemptsPerChallenge = 6
)

var verificationCodePattern = regexp.MustCompile(`^[0-9A-Za-z-]{4,16}$`)

var ErrInvalidLANAddress = errors.New("invalid LAN login address")

type Challenge struct {
	ID          string              `json:"id"`
	AccountID   string              `json:"account_id"`
	LocalURL    string              `json:"local_url"`
	LANURL      string              `json:"lan_url,omitempty"`
	LinkQRPath  string              `json:"link_qr_path"`
	State       driver.RuntimeState `json:"state"`
	Kind        driver.AuthKind     `json:"kind,omitempty"`
	Prompt      string              `json:"prompt,omitempty"`
	ExpiresAt   time.Time           `json:"expires_at"`
	CompletedAt *time.Time          `json:"completed_at,omitempty"`
}

type entry struct {
	mu                   sync.Mutex
	public               Challenge
	token                string
	driver               driver.Driver
	server               *http.Server
	listener             net.Listener
	codeAttempts         int
	codeInFlight         bool
	codeSubmitted        bool
	actionAttempts       map[string]int
	totalActionAttempts  int
	actionInFlight       bool
	performedActions     map[string]bool
	performedReplayKeys  map[string]bool
	lastObservedAt       time.Time
	onlineCandidateSince time.Time
	onlineObservations   int
	closed               bool
	done                 chan struct{}
}

type Manager struct {
	mu      sync.RWMutex
	paths   config.Paths
	entries map[string]*entry
}

func NewManager(paths config.Paths) *Manager {
	return &Manager{paths: paths, entries: make(map[string]*entry)}
}

func (m *Manager) Begin(ctx context.Context, accountID string, instance driver.Driver, lan bool, requestedLANAddress string) (Challenge, error) {
	if instance == nil {
		return Challenge{}, errors.New("driver is required")
	}
	challengeID := randomToken(16)
	token := randomToken(12)
	bind := "127.0.0.1:0"
	lanAddress, err := ResolveLANAddress(lan, requestedLANAddress)
	if err != nil {
		return Challenge{}, err
	}
	if lanAddress != "" {
		bind = net.JoinHostPort(lanAddress, "0")
	}
	listener, err := net.Listen("tcp4", bind)
	if err != nil {
		return Challenge{}, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	path := "/a/" + token
	localHost := "127.0.0.1"
	if lanAddress != "" {
		localHost = lanAddress
	}
	localURL := fmt.Sprintf("http://%s:%d%s", localHost, port, path)
	lanURL := ""
	if lan {
		lanURL = localURL
	}
	link := localURL
	if lanURL != "" {
		link = lanURL
	}
	challengeDir := filepath.Join(m.paths.Runtime, "auth", challengeID)
	if err := os.MkdirAll(challengeDir, 0o700); err != nil {
		_ = listener.Close()
		return Challenge{}, err
	}
	qrPath := filepath.Join(challengeDir, "link.png")
	if err := qrcode.WriteFile(link, qrcode.Medium, 384, qrPath); err != nil {
		_ = listener.Close()
		return Challenge{}, err
	}
	_ = os.Chmod(qrPath, 0o600)
	snapshot, _ := instance.AuthSnapshot(ctx)
	now := time.Now().UTC()
	public := Challenge{
		ID: challengeID, AccountID: accountID, LocalURL: localURL, LANURL: lanURL,
		LinkQRPath: qrPath, State: snapshot.State, Kind: snapshot.Kind, Prompt: snapshot.Prompt,
		ExpiresAt: now.Add(challengeTTL),
	}
	item := &entry{
		public: public, token: token, driver: instance, listener: listener,
		actionAttempts: make(map[string]int), performedActions: make(map[string]bool),
		performedReplayKeys: make(map[string]bool),
		done:                make(chan struct{}),
	}
	item.observeAuthSnapshotLocked(snapshot, now)
	public = item.public
	mux := http.NewServeMux()
	mux.HandleFunc(path, item.handlePage)
	mux.HandleFunc(path+"/state", item.handleState)
	mux.HandleFunc(path+"/image", item.handleImage)
	mux.HandleFunc(path+"/submit", item.handleSubmit)
	mux.HandleFunc(path+"/action", item.handleAction)
	item.server = &http.Server{
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	m.mu.Lock()
	for id, existing := range m.entries {
		if existing.public.AccountID == accountID {
			go existing.close()
			delete(m.entries, id)
		}
	}
	m.entries[challengeID] = item
	m.mu.Unlock()
	go func() {
		_ = item.server.Serve(listener)
	}()
	go m.monitor(challengeID, item)
	return public, nil
}

func (m *Manager) Status(id string) (Challenge, error) {
	m.mu.RLock()
	item := m.entries[id]
	m.mu.RUnlock()
	if item == nil {
		return Challenge{}, os.ErrNotExist
	}
	item.mu.Lock()
	defer item.mu.Unlock()
	if item.closed {
		return Challenge{}, os.ErrNotExist
	}
	now := time.Now().UTC()
	if item.completedLocked() {
		item.markCompletedLocked(now)
	}
	if now.After(item.public.ExpiresAt) && !item.completedLocked() {
		return Challenge{}, os.ErrNotExist
	}
	return item.public, nil
}

// SubmitCode keeps verification secrets out of process arguments while still
// supporting SSH-only hosts. Callers should read the code from a TTY or stdin.
func (m *Manager) SubmitCode(ctx context.Context, id, code string) error {
	m.mu.RLock()
	item := m.entries[id]
	m.mu.RUnlock()
	if item == nil {
		return os.ErrNotExist
	}
	return item.submitCode(ctx, code)
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, item := range m.entries {
		item.close()
		delete(m.entries, id)
	}
}

func (m *Manager) monitor(id string, item *entry) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		m.mu.Lock()
		delete(m.entries, id)
		m.mu.Unlock()
		item.close()
		_ = os.RemoveAll(filepath.Dir(item.public.LinkQRPath))
	}()
	for {
		select {
		case <-item.done:
			return
		case <-ticker.C:
		}
		now := time.Now().UTC()
		item.mu.Lock()
		if item.closed {
			item.mu.Unlock()
			return
		}
		if item.completedLocked() {
			item.markCompletedLocked(now)
			completedAt := *item.public.CompletedAt
			item.mu.Unlock()
			item.waitForCompletedRetention(completedAt)
			return
		}
		if now.After(item.public.ExpiresAt) {
			item.public.State = driver.StateOffline
			item.public.Prompt = "authentication challenge expired"
			item.mu.Unlock()
			return
		}
		item.mu.Unlock()
		snapshotCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		snapshot, err := item.driver.AuthSnapshot(snapshotCtx)
		cancel()
		if err != nil {
			continue
		}
		now = time.Now().UTC()
		item.mu.Lock()
		if item.closed {
			item.mu.Unlock()
			return
		}
		if item.completedLocked() {
			item.markCompletedLocked(now)
			completedAt := *item.public.CompletedAt
			item.mu.Unlock()
			item.waitForCompletedRetention(completedAt)
			return
		}
		if now.After(item.public.ExpiresAt) {
			item.public.State = driver.StateOffline
			item.public.Prompt = "authentication challenge expired"
			item.mu.Unlock()
			return
		}
		if item.observeAuthSnapshotLocked(snapshot, now) {
			completedAt := *item.public.CompletedAt
			item.mu.Unlock()
			item.waitForCompletedRetention(completedAt)
			return
		}
		item.mu.Unlock()
	}
}

func (e *entry) completedLocked() bool {
	return e.public.CompletedAt != nil
}

func (e *entry) markCompletedLocked(now time.Time) {
	e.public.State = driver.StateOnline
	if e.public.CompletedAt == nil {
		completedAt := now.UTC()
		e.public.CompletedAt = &completedAt
	}
}

func (e *entry) observeAuthSnapshotLocked(snapshot driver.AuthSnapshot, now time.Time) bool {
	e.public.Kind = snapshot.Kind
	e.lastObservedAt = snapshot.ObservedAt
	if e.lastObservedAt.IsZero() {
		e.lastObservedAt = now
	}
	if snapshot.State != driver.StateOnline {
		e.onlineCandidateSince = time.Time{}
		e.onlineObservations = 0
		e.public.State = snapshot.State
		e.public.Prompt = snapshot.Prompt
		return false
	}

	if e.onlineCandidateSince.IsZero() {
		e.onlineCandidateSince = now
		e.onlineObservations = 1
	} else {
		e.onlineObservations++
	}
	if e.onlineObservations >= minimumStableOnlineObservations &&
		now.Sub(e.onlineCandidateSince) >= onlineStabilityWindow {
		e.markCompletedLocked(now)
		return true
	}

	// Do not expose the candidate as ONLINE. Both the browser and CLI treat
	// ONLINE as terminal and would otherwise close the only useful diagnostic
	// view while the official client can still roll back to authentication.
	e.public.State = driver.StateStarting
	e.public.Prompt = "官方客户端已进入主界面，正在确认登录状态稳定"
	return false
}

func (e *entry) waitForCompletedRetention(completedAt time.Time) {
	remaining := time.Until(completedAt.Add(completedRetention))
	if remaining <= 0 {
		return
	}
	timer := time.NewTimer(remaining)
	select {
	case <-e.done:
		timer.Stop()
	case <-timer.C:
	}
}

func (e *entry) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	e.mu.Lock()
	now := time.Now().UTC()
	if e.closed {
		e.mu.Unlock()
		http.Error(w, "authentication challenge expired or completed", http.StatusGone)
		return
	}
	if e.completedLocked() {
		e.markCompletedLocked(now)
	} else if now.After(e.public.ExpiresAt) {
		e.mu.Unlock()
		http.Error(w, "authentication challenge expired", http.StatusGone)
		return
	}
	data := e.public
	basePath := r.URL.Path
	e.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTemplate.Execute(w, struct {
		Challenge
		BasePath string
	}{Challenge: data, BasePath: basePath})
}

func (e *entry) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now().UTC()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		http.Error(w, "authentication challenge expired or completed", http.StatusGone)
		return
	}
	if e.completedLocked() {
		e.markCompletedLocked(now)
		data := e.public
		observedAt := e.lastObservedAt
		e.mu.Unlock()
		writeChallengeState(w, data, false, nil, observedAt)
		return
	}
	if now.After(e.public.ExpiresAt) {
		e.mu.Unlock()
		http.Error(w, "authentication challenge expired", http.StatusGone)
		return
	}
	if e.actionInFlight {
		data := e.public
		observedAt := e.lastObservedAt
		e.mu.Unlock()
		writeChallengeState(w, data, false, nil, observedAt)
		return
	}
	e.mu.Unlock()

	snapshot, err := e.driver.AuthSnapshot(r.Context())
	if err != nil {
		http.Error(w, "driver unavailable", http.StatusServiceUnavailable)
		return
	}
	now = time.Now().UTC()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		http.Error(w, "authentication challenge expired or completed", http.StatusGone)
		return
	}
	if e.completedLocked() {
		e.markCompletedLocked(now)
		data := e.public
		observedAt := e.lastObservedAt
		e.mu.Unlock()
		writeChallengeState(w, data, false, nil, observedAt)
		return
	}
	if now.After(e.public.ExpiresAt) {
		e.mu.Unlock()
		http.Error(w, "authentication challenge expired", http.StatusGone)
		return
	}
	if e.actionInFlight {
		data := e.public
		observedAt := e.lastObservedAt
		e.mu.Unlock()
		writeChallengeState(w, data, false, nil, observedAt)
		return
	}
	if e.observeAuthSnapshotLocked(snapshot, now) {
		data := e.public
		observedAt := e.lastObservedAt
		e.mu.Unlock()
		writeChallengeState(w, data, false, nil, observedAt)
		return
	}
	data := e.public
	canSubmitCode := snapshot.State == driver.StateAuthRequired && snapshot.Kind == driver.AuthSMS &&
		snapshot.CanSubmitCode && !e.codeInFlight && !e.codeSubmitted && !e.actionInFlight
	actions := e.availableAuthActionsLocked(snapshot)
	observedAt := e.lastObservedAt
	e.mu.Unlock()
	writeChallengeState(w, data, canSubmitCode, actions, observedAt)
}

func writeChallengeState(w http.ResponseWriter, challenge Challenge, canSubmitCode bool, actions []driver.AuthAction, observedAt time.Time) {
	writeJSON(w, struct {
		Challenge
		CanSubmitCode bool                `json:"can_submit_code"`
		Actions       []driver.AuthAction `json:"actions,omitempty"`
		ObservedAt    time.Time           `json:"observed_at"`
	}{
		Challenge: challenge, CanSubmitCode: canSubmitCode,
		Actions: actions, ObservedAt: observedAt,
	})
}

func (e *entry) availableAuthActionsLocked(snapshot driver.AuthSnapshot) []driver.AuthAction {
	if e.actionInFlight || e.completedLocked() || snapshot.State != driver.StateAuthRequired {
		return nil
	}
	if e.totalActionAttempts >= maxAuthActionAttemptsPerChallenge {
		return nil
	}
	counts := make(map[string]int, len(snapshot.Actions))
	replayCounts := make(map[string]int, len(snapshot.Actions))
	for _, action := range snapshot.Actions {
		counts[action.ID]++
		if replayKey, ok := authActionReplayKey(action); ok {
			replayCounts[replayKey]++
		}
	}
	result := make([]driver.AuthAction, 0, len(snapshot.Actions))
	for _, action := range snapshot.Actions {
		replayKey, validReplayKey := authActionReplayKey(action)
		if action.ID == "" || strings.TrimSpace(action.ID) != action.ID || counts[action.ID] != 1 ||
			!validReplayKey || replayCounts[replayKey] != 1 || e.performedActions[action.ID] ||
			e.performedReplayKeys[replayKey] || e.actionAttempts[action.ID] >= maxAuthActionAttemptsPerAction {
			continue
		}
		result = append(result, action)
	}
	return result
}

func authActionReplayKey(action driver.AuthAction) (string, bool) {
	key := action.ReplayKey
	if key == "" {
		// Legacy/static drivers retain their original one-ID replay scope.
		key = action.ID
	}
	if key == "" || strings.TrimSpace(key) != key || len(key) > 256 {
		return "", false
	}
	for _, character := range key {
		if character < 0x21 || character > 0x7e {
			return "", false
		}
	}
	return key, true
}

func (e *entry) handleImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actionID := ""
	if values, present := r.URL.Query()["action_id"]; present {
		if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] || len(values[0]) > 512 {
			http.Error(w, "invalid authentication image binding", http.StatusBadRequest)
			return
		}
		actionID = values[0]
	}
	e.mu.Lock()
	unavailable := e.closed || time.Now().UTC().After(e.public.ExpiresAt) || e.completedLocked()
	e.mu.Unlock()
	if unavailable {
		http.Error(w, "login image is no longer available", http.StatusGone)
		return
	}
	snapshot, err := e.driver.AuthSnapshot(r.Context())
	if err != nil {
		http.Error(w, "driver unavailable", http.StatusServiceUnavailable)
		return
	}
	e.mu.Lock()
	now := time.Now().UTC()
	e.observeAuthSnapshotLocked(snapshot, now)
	// Never serve pixels from an authenticated main window through a login
	// endpoint, including while ONLINE is still only a stability candidate.
	unavailable = e.closed || now.After(e.public.ExpiresAt) || e.completedLocked() ||
		snapshot.State == driver.StateOnline
	e.mu.Unlock()
	if unavailable {
		http.Error(w, "login image is no longer available", http.StatusGone)
		return
	}
	var image []byte
	if actionID != "" {
		replayKey, validBinding := uniqueImageBoundAction(snapshot, actionID)
		e.mu.Lock()
		consumedBinding := e.actionInFlight || e.performedActions[actionID] || e.performedReplayKeys[replayKey]
		e.mu.Unlock()
		if !validBinding || consumedBinding || len(snapshot.ScreenshotPNG) == 0 {
			http.Error(w, "authentication image binding is stale", http.StatusConflict)
			return
		}
		image = snapshot.ScreenshotPNG
	} else {
		image = snapshot.QRCodePNG
		if len(image) == 0 {
			image = snapshot.ScreenshotPNG
		}
	}
	if len(image) == 0 {
		http.Error(w, "login image not available", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(image)
}

func uniqueImageBoundAction(snapshot driver.AuthSnapshot, actionID string) (string, bool) {
	if snapshot.State != driver.StateAuthRequired {
		return "", false
	}
	matches := 0
	targetReplayKey := ""
	for _, action := range snapshot.Actions {
		if action.ID != actionID {
			continue
		}
		if !action.ImageBound {
			return "", false
		}
		replayKey, ok := authActionReplayKey(action)
		if !ok {
			return "", false
		}
		targetReplayKey = replayKey
		matches++
	}
	if matches != 1 {
		return "", false
	}
	replayMatches := 0
	for _, action := range snapshot.Actions {
		replayKey, ok := authActionReplayKey(action)
		if ok && replayKey == targetReplayKey {
			replayMatches++
		}
	}
	return targetReplayKey, replayMatches == 1
}

func (e *entry) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOriginJSONRequest(r, "X-WeChatCopilot-Code", "user-entered") {
		http.Error(w, "cross-origin verification submission rejected", http.StatusForbidden)
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "invalid verification code", http.StatusBadRequest)
		return
	}
	if err := e.submitCode(r.Context(), body.Code); err != nil {
		if errors.Is(err, errCodeInFlight) || errors.Is(err, errCodeAlreadySubmitted) {
			http.Error(w, "verification submission is unavailable", http.StatusConflict)
			return
		}
		if errors.Is(err, errTooManyAttempts) {
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		}
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "authentication challenge expired or completed", http.StatusGone)
			return
		}
		http.Error(w, "verification failed", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"accepted": true})
}

func (e *entry) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAuthActionFailure(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "登录操作请求方式无效", false)
		return
	}
	if !sameOriginJSONRequest(r, "X-WeChatCopilot-Action", "user-confirmed") {
		writeAuthActionFailure(w, http.StatusForbidden, "REQUEST_REJECTED", "登录确认请求未通过本机来源校验", false)
		return
	}
	var body struct {
		ActionID  string `json:"action_id"`
		Confirmed bool   `json:"confirmed"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.ActionID) == "" {
		writeAuthActionFailure(w, http.StatusBadRequest, "INVALID_ACTION", "登录确认请求无效", false)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAuthActionFailure(w, http.StatusBadRequest, "INVALID_ACTION", "登录确认请求无效", false)
		return
	}
	if err := e.performAuthAction(r.Context(), body.ActionID, body.Confirmed); err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			writeAuthActionFailure(w, http.StatusGone, "CHALLENGE_GONE", "本次登录页面已完成或过期，请重新运行登录命令", false)
		case errors.Is(err, errTooManyActionAttempts):
			writeAuthActionFailure(w, http.StatusTooManyRequests, "ATTEMPT_LIMIT", "本次登录页面的确认次数已达上限，请重新运行登录命令", false)
		case errors.Is(err, errActionInFlight):
			writeAuthActionFailure(w, http.StatusConflict, "ACTION_IN_PROGRESS", "上一项登录操作仍在处理中，请稍候", true)
		case errors.Is(err, errConfirmationRequired):
			writeAuthActionFailure(w, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "该操作必须由你本人明确确认", true)
		case errors.Is(err, errActionUnavailable):
			writeAuthActionFailure(w, http.StatusConflict, "ACTION_NOT_CURRENT", "该按钮已过期或已使用；请等待画面刷新，必要时重新运行登录命令", true)
		default:
			writeClassifiedAuthActionFailure(w, err)
		}
		return
	}
	writeJSON(w, map[string]bool{"accepted": true})
}

func writeAuthActionFailure(w http.ResponseWriter, status int, code, message string, retryable bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		OK    bool `json:"ok"`
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}{
		OK: false,
		Error: struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		}{Code: code, Message: message, Retryable: retryable},
	})
}

func writeClassifiedAuthActionFailure(w http.ResponseWriter, err error) {
	kind, classified := driver.ClassifyFailure(err)
	if !classified {
		writeAuthActionFailure(
			w, http.StatusConflict, "OUTCOME_UNCERTAIN",
			"操作可能已提交；为避免重复操作，本次登录页面不会重放该操作，请观察官方客户端或重新运行登录命令",
			false,
		)
		return
	}
	switch kind {
	case driver.FailureStale:
		writeAuthActionFailure(w, http.StatusConflict, "PAGE_CHANGED", "官方客户端页面在确认前发生变化；画面刷新后可再次确认", true)
	case driver.FailureTargetAmbiguous:
		writeAuthActionFailure(w, http.StatusConflict, "TARGET_AMBIGUOUS", "当前登录控件不唯一；请保持官方客户端仅显示一个登录窗口", true)
	case driver.FailureClientIncompatible:
		writeAuthActionFailure(w, http.StatusConflict, "CLIENT_INCOMPATIBLE", "当前官方客户端登录控件无法安全操作", false)
	case driver.FailureUserActionRequired:
		writeAuthActionFailure(w, http.StatusConflict, "USER_ACTION_REQUIRED", "该步骤只能在官方客户端中由你本人完成", false)
	case driver.FailureAuthRequired:
		writeAuthActionFailure(w, http.StatusConflict, "AUTH_SCREEN_CHANGED", "官方客户端已切换到另一种登录验证页面；请按新画面继续", true)
	case driver.FailureInvalidArgument:
		writeAuthActionFailure(w, http.StatusBadRequest, "INVALID_ACTION", "登录确认请求与当前客户端页面不匹配", false)
	case driver.FailureDriverUnavailable:
		writeAuthActionFailure(w, http.StatusServiceUnavailable, "DRIVER_UNAVAILABLE", "官方客户端运行时暂时不可用", true)
	default:
		writeAuthActionFailure(w, http.StatusConflict, "ACTION_UNAVAILABLE", "当前登录操作不可用", false)
	}
}

var (
	errTooManyAttempts      = errors.New("too many verification attempts")
	errCodeInFlight         = errors.New("verification submission is already in progress")
	errCodeAlreadySubmitted = errors.New("verification code was already submitted for this challenge")
)

var (
	errTooManyActionAttempts = errors.New("too many authentication action attempts")
	errActionInFlight        = errors.New("authentication action is already in progress")
	errConfirmationRequired  = errors.New("explicit user confirmation is required")
	errActionUnavailable     = errors.New("authentication action is unavailable")
)

func (e *entry) submitCode(ctx context.Context, code string) (err error) {
	if !verificationCodePattern.MatchString(code) {
		return errors.New("invalid verification code")
	}
	e.mu.Lock()
	if e.closed || time.Now().UTC().After(e.public.ExpiresAt) || e.completedLocked() {
		e.mu.Unlock()
		return os.ErrNotExist
	}
	if e.codeInFlight {
		e.mu.Unlock()
		return errCodeInFlight
	}
	if e.codeSubmitted {
		e.mu.Unlock()
		return errCodeAlreadySubmitted
	}
	e.codeAttempts++
	attempts := e.codeAttempts
	if attempts > 8 {
		e.mu.Unlock()
		return errTooManyAttempts
	}
	e.codeInFlight = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.codeInFlight = false
		if err == nil {
			e.codeSubmitted = true
		}
		e.mu.Unlock()
	}()

	snapshot, err := e.driver.AuthSnapshot(ctx)
	if err != nil {
		return err
	}
	if snapshot.State != driver.StateAuthRequired || snapshot.Kind != driver.AuthSMS || !snapshot.CanSubmitCode {
		return errors.New("verification code input is not available on the current official login screen")
	}
	return e.driver.SubmitAuthCode(ctx, code)
}

func (e *entry) performAuthAction(ctx context.Context, actionID string, confirmed bool) error {
	actionID = strings.TrimSpace(actionID)
	e.mu.Lock()
	if e.closed || time.Now().UTC().After(e.public.ExpiresAt) || e.completedLocked() {
		e.mu.Unlock()
		return os.ErrNotExist
	}
	if e.actionInFlight {
		e.mu.Unlock()
		return errActionInFlight
	}
	if e.performedActions[actionID] {
		e.mu.Unlock()
		return errActionUnavailable
	}
	if !confirmed {
		e.mu.Unlock()
		return errConfirmationRequired
	}
	e.actionInFlight = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.actionInFlight = false
		e.mu.Unlock()
	}()

	snapshot, err := e.driver.AuthSnapshot(ctx)
	if err != nil {
		return err
	}
	if snapshot.State == driver.StateOnline {
		return os.ErrNotExist
	}
	if snapshot.State != driver.StateAuthRequired {
		return errActionUnavailable
	}
	var advertised *driver.AuthAction
	for index := range snapshot.Actions {
		if snapshot.Actions[index].ID != actionID {
			continue
		}
		if advertised != nil {
			return errActionUnavailable
		}
		advertised = &snapshot.Actions[index]
	}
	if advertised == nil {
		return errActionUnavailable
	}
	replayKey, validReplayKey := authActionReplayKey(*advertised)
	if !validReplayKey {
		return errActionUnavailable
	}
	replayMatches := 0
	for _, action := range snapshot.Actions {
		candidate, ok := authActionReplayKey(action)
		if ok && candidate == replayKey {
			replayMatches++
		}
	}
	if replayMatches != 1 {
		return errActionUnavailable
	}
	actor, ok := e.driver.(driver.AuthActionDriver)
	if !ok {
		return errActionUnavailable
	}
	e.mu.Lock()
	if e.closed || time.Now().UTC().After(e.public.ExpiresAt) || e.completedLocked() {
		e.mu.Unlock()
		return os.ErrNotExist
	}
	if e.actionAttempts == nil {
		e.actionAttempts = make(map[string]int)
	}
	if e.performedActions == nil {
		e.performedActions = make(map[string]bool)
	}
	if e.performedReplayKeys == nil {
		e.performedReplayKeys = make(map[string]bool)
	}
	if e.performedActions[actionID] || e.performedReplayKeys[replayKey] {
		e.mu.Unlock()
		return errActionUnavailable
	}
	if e.actionAttempts[actionID] >= maxAuthActionAttemptsPerAction ||
		e.totalActionAttempts >= maxAuthActionAttemptsPerChallenge {
		e.mu.Unlock()
		return errTooManyActionAttempts
	}
	e.actionAttempts[actionID]++
	e.totalActionAttempts++
	// Consume both the concrete generation and its logical operation before
	// crossing into the driver. No result can prove a failed call was harmless
	// enough to replay automatically.
	e.performedActions[actionID] = true
	e.performedReplayKeys[replayKey] = true
	e.mu.Unlock()
	err = actor.PerformAuthAction(ctx, driver.AuthActionRequest{ActionID: actionID, Confirmed: confirmed})
	if err != nil && authActionDefinitivelyRejected(err) {
		// The action was reserved before crossing the driver boundary so a
		// concurrent refreshed generation could never race it. A classified,
		// non-consumed rejection is the only outcome that proves no side effect
		// occurred; release just the replay tombstones while retaining bounded
		// attempt accounting.
		e.mu.Lock()
		delete(e.performedActions, actionID)
		delete(e.performedReplayKeys, replayKey)
		e.mu.Unlock()
	}
	return err
}

func authActionDefinitivelyRejected(err error) bool {
	if err == nil || driver.AuthActionWasConsumed(err) {
		return false
	}
	kind, ok := driver.ClassifyFailure(err)
	if !ok {
		return false
	}
	switch kind {
	case driver.FailureInvalidArgument, driver.FailureAuthRequired,
		driver.FailureClientIncompatible, driver.FailureTargetAmbiguous,
		driver.FailureUserActionRequired, driver.FailureStale,
		driver.FailureDriverUnavailable:
		return true
	default:
		return false
	}
}

func sameOriginJSONRequest(r *http.Request, markerHeader, markerValue string) bool {
	mediaType := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
	if !strings.EqualFold(mediaType, "application/json") || r.Header.Get(markerHeader) != markerValue {
		return false
	}
	origin, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && origin.Scheme == "http" && origin.Host == r.Host && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == ""
}

func (e *entry) close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	close(e.done)
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = e.server.Shutdown(ctx)
	_ = e.listener.Close()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

type lanInterface struct {
	Name      string
	Index     int
	Flags     net.Flags
	Addresses []net.IP
	Excluded  bool
}

// ResolveLANAddress validates an explicit LAN bind or selects a safe local
// RFC1918 address. A configured address is never considered without lan=true.
func ResolveLANAddress(lan bool, requested string) (string, error) {
	if !lan {
		if requested != "" {
			return "", fmt.Errorf("%w: an address requires LAN login to be enabled", ErrInvalidLANAddress)
		}
		return "", nil
	}
	if requested == "" {
		requested = os.Getenv(config.EnvLANAddress)
	}
	return privateLANAddress(requested)
}

func privateLANAddress(requested string) (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("list network interfaces: %w", err)
	}
	candidates := make([]lanInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		candidate := lanInterface{
			Name: iface.Name, Index: iface.Index, Flags: iface.Flags, Excluded: excludedLANInterface(iface.Name),
		}
		addresses, addressErr := iface.Addrs()
		if addressErr == nil {
			for _, address := range addresses {
				var ip net.IP
				switch value := address.(type) {
				case *net.IPNet:
					ip = value.IP
				case *net.IPAddr:
					ip = value.IP
				}
				if ipv4 := ip.To4(); ipv4 != nil {
					candidate.Addresses = append(candidate.Addresses, append(net.IP(nil), ipv4...))
				}
			}
		}
		candidates = append(candidates, candidate)
	}
	return selectPrivateLANAddress(requested, candidates, defaultRouteInterfaces("/proc/net/route"))
}

func selectPrivateLANAddress(requested string, interfaces []lanInterface, defaultInterfaces []string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		ip := net.ParseIP(requested)
		ipv4 := ip.To4()
		if ipv4 == nil || !isRFC1918(ipv4) || ip.IsUnspecified() || ip.IsLoopback() {
			return "", fmt.Errorf("%w: %q must be an assigned RFC1918 IPv4 address", ErrInvalidLANAddress, requested)
		}
		for _, iface := range interfaces {
			if !eligibleLANInterface(iface) {
				continue
			}
			for _, assigned := range iface.Addresses {
				if assigned.To4() != nil && assigned.To4().Equal(ipv4) {
					return ipv4.String(), nil
				}
			}
		}
		return "", fmt.Errorf("%w: %q is not assigned to an eligible local interface", ErrInvalidLANAddress, requested)
	}

	byName := make(map[string]lanInterface, len(interfaces))
	for _, iface := range interfaces {
		byName[iface.Name] = iface
	}
	ordered := make([]lanInterface, 0, len(interfaces))
	seen := make(map[string]bool, len(interfaces))
	for _, name := range defaultInterfaces {
		if iface, ok := byName[name]; ok && !seen[name] {
			ordered = append(ordered, iface)
			seen[name] = true
		}
	}
	rest := append([]lanInterface(nil), interfaces...)
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].Index == rest[j].Index {
			return rest[i].Name < rest[j].Name
		}
		return rest[i].Index < rest[j].Index
	})
	for _, iface := range rest {
		if !seen[iface.Name] {
			ordered = append(ordered, iface)
		}
	}
	for _, iface := range ordered {
		if !eligibleLANInterface(iface) {
			continue
		}
		addresses := append([]net.IP(nil), iface.Addresses...)
		sort.Slice(addresses, func(i, j int) bool { return bytesCompareIPv4(addresses[i], addresses[j]) < 0 })
		for _, ip := range addresses {
			if ipv4 := ip.To4(); ipv4 != nil && isRFC1918(ipv4) {
				return ipv4.String(), nil
			}
		}
	}
	return "", fmt.Errorf("%w: no RFC1918 address is assigned to an eligible local interface", ErrInvalidLANAddress)
}

func eligibleLANInterface(iface lanInterface) bool {
	return iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 && !iface.Excluded
}

func excludedLANInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{"docker", "veth", "br-", "virbr", "cni", "flannel"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	_, err := os.Stat(filepath.Join("/sys/class/net", name, "bridge"))
	return err == nil
}

func isRFC1918(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil {
		return false
	}
	return ip[0] == 10 || (ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) || (ip[0] == 192 && ip[1] == 168)
}

func bytesCompareIPv4(left, right net.IP) int {
	left = left.To4()
	right = right.To4()
	for index := 0; index < net.IPv4len; index++ {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

type defaultRoute struct {
	name   string
	metric int
}

func defaultRouteInterfaces(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	var routes []defaultRoute
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&1 == 0 {
			continue
		}
		metric, err := strconv.Atoi(fields[6])
		if err != nil {
			continue
		}
		routes = append(routes, defaultRoute{name: fields[0], metric: metric})
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].metric == routes[j].metric {
			return routes[i].name < routes[j].name
		}
		return routes[i].metric < routes[j].metric
	})
	result := make([]string, 0, len(routes))
	for _, route := range routes {
		result = append(result, route.name)
	}
	return result
}

func randomToken(bytes int) string {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

var pageTemplate = template.Must(template.New("auth").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>wechatcopilot login</title><style>
:root{color-scheme:light;background:#f2efe8;color:#17211d;font-family:ui-sans-serif,system-ui,sans-serif}*{box-sizing:border-box}body{margin:0;display:grid;min-height:100vh;place-items:center;padding:16px}.card{width:min(100%,576px);background:#fff;border:1px solid #d7d1c5;border-radius:20px;padding:28px;box-shadow:0 20px 60px #263a3020}h1{margin:0 0 8px;font-size:26px}p{color:#59645f}.screen-link{display:flex;width:100%;max-width:420px;max-height:70vh;margin:20px auto;border-radius:12px;background:#e7e4dc}.screen{display:block;width:auto;height:auto;max-width:100%;max-height:70vh;object-fit:contain;margin:auto;border-radius:12px}.actions{display:grid;gap:10px;margin:14px 0}.actions p{margin:0;font-size:13px}.auth-action{background:#9a5b00}form{display:flex;gap:10px}input,button{font:inherit;padding:12px;border-radius:10px;border:1px solid #b8beb9}input{min-width:0;flex:1}button{background:#087f5b;color:#fff;border:0;font-weight:700;cursor:pointer}button:disabled{cursor:wait;opacity:.6}[hidden]{display:none!important}@media(max-width:420px){.card{padding:20px}form{flex-wrap:wrap}button{width:100%}}.meta{font-family:ui-monospace,monospace;font-size:12px;overflow-wrap:anywhere}</style></head>
<body><main class="card"><h1>完成账号登录</h1><p id="prompt">{{.Prompt}}</p><a class="screen-link" href="{{.BasePath}}/image" target="_blank" rel="noopener" aria-label="Open login image at full size"><img class="screen" src="{{.BasePath}}/image" alt="Login QR or client screen"></a>
<section id="actions" class="actions" hidden><p>以下操作必须由你本人明确确认：</p><div id="action-buttons"></div></section>
<form id="code" hidden><input name="code" inputmode="numeric" autocomplete="one-time-code" placeholder="手机验证码"><button>提交</button></form><p class="meta">Challenge {{.ID}} · 页面只在当前登录挑战期间有效</p></main>
<script>
const base=location.pathname;
const prompt=document.querySelector('#prompt');
const form=document.querySelector('#code');
const codeButton=form.querySelector('button');
const actions=document.querySelector('#actions');
const actionButtons=document.querySelector('#action-buttons');
let screen=document.querySelector('.screen');
const screenLink=document.querySelector('.screen-link');
let actionSignature='';
let imageBoundActionID='';
let imageRefreshes=0;
let imageGeneration=0;
let uiGeneration=0;
let stateInFlight=false;
let pollTimer=0;
let completed=false;
let actionNotice='';
let actionNoticeUntil=0;
function refreshImage(){
  if(completed||imageBoundActionID)return;
  const generation=++imageGeneration;
  const source=base+'/image?v='+Date.now();
  const candidate=new Image();
  candidate.className='screen';
  candidate.alt=screen.alt;
  candidate.onload=()=>{if(completed||generation!==imageGeneration)return;screen.replaceWith(candidate);screen=candidate;screenLink.href=source;screenLink.hidden=false};
  candidate.onerror=()=>{};
  candidate.src=source;
}
function scheduleState(delay=1500){if(completed)return;clearTimeout(pollTimer);pollTimer=setTimeout(refreshState,delay)}
async function performAction(action,button){
  if(action.requires_confirmation&&!window.confirm(action.confirmation||'请确认后继续。'))return;
  const generation=++uiGeneration;
  button.disabled=true;
  try{
    const response=await fetch(base+'/action',{method:'POST',headers:{'content-type':'application/json','X-WeChatCopilot-Action':'user-confirmed'},body:JSON.stringify({action_id:action.id,confirmed:true})});
    if(completed||generation!==uiGeneration)return;
    let failure=null;
    if(!response.ok){try{failure=await response.json()}catch(_error){failure=null}}
    if(completed||generation!==uiGeneration)return;
    actionNotice=response.ok?'操作已提交，正在刷新官方客户端画面':(failure&&failure.error&&failure.error.message)||'当前登录操作不可用';
    actionNoticeUntil=Date.now()+(response.ok?3000:10000);
    prompt.textContent=actionNotice;
    if(response.ok){actionSignature='';imageBoundActionID='';refreshImage();scheduleState(0)}else{button.disabled=false;actionSignature='';imageBoundActionID='';scheduleState(0)}
  }catch(_error){if(!completed&&generation===uiGeneration){prompt.textContent='无法连接登录服务，请稍后重试';button.disabled=false;scheduleState(0)}}
}
function populateActionButtons(available){
  actionButtons.replaceChildren();
  for(const action of available){const button=document.createElement('button');button.type='button';button.className='auth-action';button.textContent=action.label;button.addEventListener('click',()=>performAction(action,button));actionButtons.append(button)}
  actions.hidden=available.length===0;
}
function renderActions(values){
  const available=Array.isArray(values)?values:[];
  const signature=JSON.stringify(available);
  if(signature===actionSignature)return;
  actionSignature=signature;
  imageGeneration++;
  const hadImageBound=imageBoundActionID!=='';
  imageBoundActionID='';
  const bound=available.filter(action=>action&&action.image_bound===true);
  if(bound.length===0){populateActionButtons(available);if(hadImageBound)refreshImage();return}
  actionButtons.replaceChildren();
  actions.hidden=true;
  screenLink.hidden=true;
  if(bound.length!==available.length)return;
  const action=bound[0];
  const binding=JSON.stringify(bound.map(value=>value.id));
  imageBoundActionID=binding;
  const generation=++imageGeneration;
  const source=base+'/image?action_id='+encodeURIComponent(action.id)+'&v='+Date.now();
  const candidate=new Image();
  candidate.className='screen';
  candidate.alt=screen.alt;
  candidate.onload=()=>{if(completed||generation!==imageGeneration||signature!==actionSignature||imageBoundActionID!==binding)return;screen.replaceWith(candidate);screen=candidate;screenLink.href=source;screenLink.hidden=false;populateActionButtons(available)};
  candidate.onerror=()=>{if(completed||generation!==imageGeneration||signature!==actionSignature)return;actionSignature='';imageBoundActionID='';actions.hidden=true};
  candidate.src=source;
}
async function refreshState(){
  if(completed||stateInFlight)return;
  stateInFlight=true;
  const generation=uiGeneration;
  try{
    const response=await fetch(base+'/state',{cache:'no-store'});
    if(!response.ok||completed||generation!==uiGeneration)return;
    const state=await response.json();
    if(completed||generation!==uiGeneration)return;
    if(state.state==='ONLINE'){
      completed=true;
      imageGeneration++;
      clearTimeout(pollTimer);
      prompt.textContent='登录完成，可以关闭此页面';
      form.hidden=true;
      renderActions([]);
      screenLink.hidden=true;
      return;
    }
    if(Date.now()>=actionNoticeUntil){actionNotice='';prompt.textContent=state.prompt||state.state}
    form.hidden=!state.can_submit_code;
    codeButton.disabled=!state.can_submit_code;
    renderActions(state.actions);
    imageRefreshes++;
    if(imageRefreshes%3===0)refreshImage();
  }catch(_error){
    if(!completed&&generation===uiGeneration)prompt.textContent='登录服务暂时不可用，正在重试';
  }finally{
    stateInFlight=false;
    scheduleState();
  }
}
form.addEventListener('submit',async event=>{event.preventDefault();if(codeButton.disabled)return;const generation=++uiGeneration;const code=new FormData(event.target).get('code');codeButton.disabled=true;try{const response=await fetch(base+'/submit',{method:'POST',headers:{'content-type':'application/json','X-WeChatCopilot-Code':'user-entered'},body:JSON.stringify({code})});if(completed||generation!==uiGeneration)return;prompt.textContent=response.ok?'验证码已提交，请在手机上完成确认':'验证码无效、当前页面不接受验证码或挑战已过期';if(response.ok){form.hidden=true}else{codeButton.disabled=false}}catch(_error){if(!completed&&generation===uiGeneration){prompt.textContent='无法提交验证码，请稍后重试';codeButton.disabled=false}}finally{scheduleState(0)}});
refreshState();
</script></body></html>`))
