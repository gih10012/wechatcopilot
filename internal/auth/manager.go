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
	// Three onboarding stages may each make one bounded retry before the
	// challenge fails closed.
	maxAuthActionAttemptsPerAction    = 2
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
	mu                  sync.Mutex
	public              Challenge
	token               string
	driver              driver.Driver
	server              *http.Server
	listener            net.Listener
	codeAttempts        int
	codeInFlight        bool
	codeSubmitted       bool
	actionAttempts      map[string]int
	totalActionAttempts int
	actionInFlight      bool
	performedActions    map[string]bool
	lastObservedAt      time.Time
	closed              bool
	done                chan struct{}
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
	public := Challenge{
		ID: challengeID, AccountID: accountID, LocalURL: localURL, LANURL: lanURL,
		LinkQRPath: qrPath, State: snapshot.State, Kind: snapshot.Kind, Prompt: snapshot.Prompt,
		ExpiresAt: time.Now().UTC().Add(challengeTTL),
	}
	if snapshot.State == driver.StateOnline {
		now := time.Now().UTC()
		public.CompletedAt = &now
	}
	item := &entry{
		public: public, token: token, driver: instance, listener: listener,
		actionAttempts: make(map[string]int), performedActions: make(map[string]bool),
		lastObservedAt: snapshot.ObservedAt, done: make(chan struct{}),
	}
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
		item.public.State = snapshot.State
		item.public.Kind = snapshot.Kind
		item.public.Prompt = snapshot.Prompt
		item.lastObservedAt = snapshot.ObservedAt
		if snapshot.State == driver.StateOnline {
			item.markCompletedLocked(now)
			completedAt := *item.public.CompletedAt
			item.mu.Unlock()
			item.waitForCompletedRetention(completedAt)
			return
		}
		item.mu.Unlock()
	}
}

func (e *entry) completedLocked() bool {
	return e.public.CompletedAt != nil || e.public.State == driver.StateOnline
}

func (e *entry) markCompletedLocked(now time.Time) {
	e.public.State = driver.StateOnline
	if e.public.CompletedAt == nil {
		completedAt := now.UTC()
		e.public.CompletedAt = &completedAt
	}
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
	e.public.Kind = snapshot.Kind
	e.public.Prompt = snapshot.Prompt
	e.lastObservedAt = snapshot.ObservedAt
	if snapshot.State == driver.StateOnline {
		e.markCompletedLocked(now)
		data := e.public
		observedAt := e.lastObservedAt
		e.mu.Unlock()
		writeChallengeState(w, data, false, nil, observedAt)
		return
	}
	e.public.State = snapshot.State
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
	for _, action := range snapshot.Actions {
		counts[action.ID]++
	}
	result := make([]driver.AuthAction, 0, len(snapshot.Actions))
	for _, action := range snapshot.Actions {
		if action.ID == "" || strings.TrimSpace(action.ID) != action.ID || counts[action.ID] != 1 ||
			e.performedActions[action.ID] || e.actionAttempts[action.ID] >= maxAuthActionAttemptsPerAction {
			continue
		}
		result = append(result, action)
	}
	return result
}

func (e *entry) handleImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
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
	if snapshot.State == driver.StateOnline {
		e.markCompletedLocked(time.Now().UTC())
	}
	unavailable = e.closed || time.Now().UTC().After(e.public.ExpiresAt) || e.completedLocked()
	e.mu.Unlock()
	if unavailable {
		http.Error(w, "login image is no longer available", http.StatusGone)
		return
	}
	image := snapshot.QRCodePNG
	if len(image) == 0 {
		image = snapshot.ScreenshotPNG
	}
	if len(image) == 0 {
		http.Error(w, "login image not available", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(image)
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOriginJSONRequest(r, "X-WeChatCopilot-Action", "user-confirmed") {
		http.Error(w, "cross-origin authentication action rejected", http.StatusForbidden)
		return
	}
	var body struct {
		ActionID  string `json:"action_id"`
		Confirmed bool   `json:"confirmed"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.ActionID) == "" {
		http.Error(w, "invalid authentication action", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid authentication action", http.StatusBadRequest)
		return
	}
	if err := e.performAuthAction(r.Context(), body.ActionID, body.Confirmed); err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			http.Error(w, "authentication challenge expired or completed", http.StatusGone)
		case errors.Is(err, errTooManyActionAttempts):
			http.Error(w, "too many action attempts", http.StatusTooManyRequests)
		case errors.Is(err, errActionInFlight):
			http.Error(w, "authentication action already in progress", http.StatusConflict)
		case errors.Is(err, errConfirmationRequired):
			http.Error(w, "explicit confirmation is required", http.StatusBadRequest)
		default:
			http.Error(w, "authentication action is unavailable", http.StatusConflict)
		}
		return
	}
	writeJSON(w, map[string]bool{"accepted": true})
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

func (e *entry) performAuthAction(ctx context.Context, actionID string, confirmed bool) (err error) {
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
		if err == nil || driver.AuthActionWasConsumed(err) {
			if e.performedActions == nil {
				e.performedActions = make(map[string]bool)
			}
			e.performedActions[actionID] = true
		}
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
	if e.actionAttempts[actionID] >= maxAuthActionAttemptsPerAction ||
		e.totalActionAttempts >= maxAuthActionAttemptsPerChallenge {
		e.mu.Unlock()
		return errTooManyActionAttempts
	}
	e.actionAttempts[actionID]++
	e.totalActionAttempts++
	e.mu.Unlock()
	return actor.PerformAuthAction(ctx, driver.AuthActionRequest{ActionID: actionID, Confirmed: confirmed})
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
let imageRefreshes=0;
let imageGeneration=0;
let uiGeneration=0;
let stateInFlight=false;
let pollTimer=0;
let completed=false;
function refreshImage(){
  if(completed)return;
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
    prompt.textContent=response.ok?'操作已提交，正在刷新官方客户端画面':'操作不可用；请确认页面未变化后重试';
    if(response.ok){actionSignature='';refreshImage();scheduleState(0)}else{button.disabled=false}
  }catch(_error){if(!completed&&generation===uiGeneration){prompt.textContent='无法连接登录服务，请稍后重试';button.disabled=false;scheduleState(0)}}
}
function renderActions(values){
  const available=Array.isArray(values)?values:[];
  const signature=JSON.stringify(available);
  if(signature===actionSignature)return;
  actionSignature=signature;
  actionButtons.replaceChildren();
  for(const action of available){const button=document.createElement('button');button.type='button';button.className='auth-action';button.textContent=action.label;button.addEventListener('click',()=>performAction(action,button));actionButtons.append(button)}
  actions.hidden=available.length===0;
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
    prompt.textContent=state.prompt||state.state;
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
