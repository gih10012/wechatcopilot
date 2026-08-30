package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/service"
)

type Server struct {
	service    *service.Service
	httpServer *http.Server
	listener   net.Listener
	socketPath string
	socket     *socketIdentity

	recoveryMu       sync.Mutex
	recoveryCancel   context.CancelFunc
	recoveryDone     chan struct{}
	recoveryStopping bool
	restore          func(context.Context) []error
	closeService     func(context.Context) error
	recoveryPolicy   restoreRetryPolicy
	recoveryReporter func(attempt, maximum int, err error)
}

type restoreRetryPolicy struct {
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	attemptTimeout time.Duration
	wait           func(context.Context, time.Duration) error
}

var defaultRestoreRetryPolicy = restoreRetryPolicy{
	maxAttempts: 10, initialBackoff: 500 * time.Millisecond, maxBackoff: 15 * time.Second,
	attemptTimeout: 45 * time.Second,
}

type socketIdentity struct {
	device uint64
	inode  uint64
	uid    uint32
}

func New(socketPath string, service *service.Service) *Server {
	server := &Server{
		service: service, socketPath: socketPath, restore: service.Restore,
		closeService: service.Close, recoveryPolicy: defaultRestoreRetryPolicy,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", server.handleHealth)
	mux.HandleFunc("GET /v1/accounts", server.handleAccounts)
	mux.HandleFunc("POST /v1/accounts/add", server.handleAccountAdd)
	mux.HandleFunc("POST /v1/accounts/status", server.handleAccountStatus)
	mux.HandleFunc("POST /v1/accounts/activate", server.handleAccountActivate)
	mux.HandleFunc("POST /v1/accounts/deactivate", server.handleAccountDeactivate)
	mux.HandleFunc("POST /v1/accounts/remove", server.handleAccountRemove)
	mux.HandleFunc("POST /v1/auth/begin", server.handleAuthBegin)
	mux.HandleFunc("POST /v1/auth/status", server.handleAuthStatus)
	mux.HandleFunc("POST /v1/auth/submit", server.handleAuthSubmit)
	mux.HandleFunc("POST /v1/capabilities", server.handleCapabilities)
	mux.HandleFunc("POST /v1/conversations/list", server.handleConversations)
	mux.HandleFunc("POST /v1/messages/list", server.handleMessages)
	mux.HandleFunc("POST /v1/messages/watch", server.handleMessagesWatch)
	mux.HandleFunc("POST /v1/messages/search", server.handleMessagesSearch)
	mux.HandleFunc("POST /v1/messages/prepare-send", server.handlePrepareSend)
	mux.HandleFunc("POST /v1/messages/commit-send", server.handleCommitSend)
	mux.HandleFunc("POST /v1/surfaces/open", server.handleSurfaceOpen)
	mux.HandleFunc("POST /v1/surfaces/snapshot", server.handleSurfaceSnapshot)
	mux.HandleFunc("POST /v1/surfaces/act", server.handleSurfaceAct)
	mux.HandleFunc("POST /v1/surfaces/export", server.handleSurfaceExport)
	mux.HandleFunc("POST /v1/surfaces/close", server.handleSurfaceClose)
	server.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       35 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	return server
}

func (s *Server) Listen() error {
	if err := validateSocketLocation(s.socketPath); err != nil {
		return err
	}
	if identity, err := ownedSocketIdentity(s.socketPath); err == nil {
		probe, dialErr := net.DialTimeout("unix", s.socketPath, 300*time.Millisecond)
		if dialErr == nil {
			_ = probe.Close()
			return fmt.Errorf("daemon already listens on %s", s.socketPath)
		}
		if err := removeOwnedSocket(s.socketPath, identity); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("refusing to replace daemon socket path: %w", err)
	}
	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return err
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		_ = listener.Close()
		return errors.New("daemon socket listener is not a Unix listener")
	}
	unixListener.SetUnlinkOnClose(false)
	identity, err := ownedSocketIdentity(s.socketPath)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("inspect new daemon socket: %w", err)
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = removeOwnedSocket(s.socketPath, identity)
		return err
	}
	s.listener = &sameUIDListener{UnixListener: unixListener, uid: uint32(os.Geteuid())}
	s.socket = &identity
	return nil
}

func (s *Server) Serve() error {
	if s.listener == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	s.startRestoreRecovery()
	err := s.httpServer.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	recoveryErr := s.stopRestoreRecovery(ctx)
	httpErr := s.httpServer.Shutdown(ctx)
	var listenerErr error
	if s.listener != nil {
		listenerErr = s.listener.Close()
		if errors.Is(listenerErr, net.ErrClosed) {
			listenerErr = nil
		}
	}
	var serviceErr error
	if recoveryErr == nil {
		serviceErr = s.closeService(ctx)
	}
	var socketErr error
	if s.socket != nil {
		socketErr = removeOwnedSocket(s.socketPath, *s.socket)
	}
	return errors.Join(recoveryErr, httpErr, listenerErr, serviceErr, socketErr)
}

// SetRestoreFailureReporter installs a non-blocking diagnostic callback. It
// must be called before Serve.
func (s *Server) SetRestoreFailureReporter(reporter func(attempt, maximum int, err error)) {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	s.recoveryReporter = reporter
}

func (s *Server) startRestoreRecovery() {
	s.recoveryMu.Lock()
	if s.recoveryDone != nil || s.recoveryStopping {
		s.recoveryMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.recoveryCancel = cancel
	s.recoveryDone = done
	restore := s.restore
	policy := s.recoveryPolicy
	reporter := s.recoveryReporter
	s.recoveryMu.Unlock()

	go func() {
		_ = runRestoreRetry(ctx, policy, restore, reporter)
		s.recoveryMu.Lock()
		close(done)
		s.recoveryMu.Unlock()
	}()
}

func (s *Server) stopRestoreRecovery(ctx context.Context) error {
	s.recoveryMu.Lock()
	s.recoveryStopping = true
	cancel := s.recoveryCancel
	done := s.recoveryDone
	s.recoveryMu.Unlock()
	if cancel == nil || done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runRestoreRetry(
	ctx context.Context,
	policy restoreRetryPolicy,
	restore func(context.Context) []error,
	reporter func(attempt, maximum int, err error),
) error {
	if policy.maxAttempts <= 0 {
		return errors.New("restore retry policy must allow at least one attempt")
	}
	if policy.maxAttempts > 1 && policy.initialBackoff <= 0 {
		return errors.New("restore retry policy must use a positive initial backoff")
	}
	if policy.maxAttempts > 1 && policy.maxBackoff <= 0 {
		return errors.New("restore retry policy must use a positive maximum backoff")
	}
	if restore == nil {
		return errors.New("restore callback is unavailable")
	}
	backoff := policy.initialBackoff
	if backoff < 0 {
		backoff = 0
	}
	if policy.maxBackoff > 0 && backoff > policy.maxBackoff {
		backoff = policy.maxBackoff
	}
	wait := policy.wait
	if wait == nil {
		wait = waitForRestoreRetry
	}
	var lastErr error
	for attempt := 1; attempt <= policy.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx := ctx
		cancel := func() {}
		if policy.attemptTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, policy.attemptTimeout)
		}
		failures := restore(attemptCtx)
		attemptErr := attemptCtx.Err()
		cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = errors.Join(errors.Join(failures...), attemptErr)
		if lastErr == nil {
			return nil
		}
		if reporter != nil {
			reporter(attempt, policy.maxAttempts, lastErr)
		}
		if attempt == policy.maxAttempts {
			return lastErr
		}
		if err := wait(ctx, backoff); err != nil {
			return err
		}
		if backoff == 0 {
			continue
		}
		next := backoff * 2
		if next < backoff || (policy.maxBackoff > 0 && next > policy.maxBackoff) {
			next = policy.maxBackoff
		}
		backoff = next
	}
	return lastErr
}

func waitForRestoreRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func ownedSocketIdentity(path string) (socketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return socketIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 {
		return socketIdentity{}, errors.New("path is not a Unix socket")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return socketIdentity{}, errors.New("Unix socket is not owned by the current user")
	}
	return socketIdentity{device: uint64(stat.Dev), inode: stat.Ino, uid: stat.Uid}, nil
}

func validateSocketLocation(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("daemon socket path must be absolute")
	}
	parent := filepath.Dir(filepath.Clean(path))
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect daemon socket directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("daemon socket directory must be a non-symlink directory")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errors.New("daemon socket directory must be owned by the current user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("daemon socket directory must not be accessible by other users")
	}
	return nil
}

func removeOwnedSocket(path string, expected socketIdentity) error {
	current, err := ownedSocketIdentity(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("refusing to remove changed daemon socket path: %w", err)
	}
	if current != expected {
		return errors.New("refusing to remove changed daemon socket inode")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove daemon socket: %w", err)
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.respond(w, map[string]any{"ready": true, "active_accounts": s.service.ActiveAccounts()}, nil)
}

func (s *Server) handleAccounts(w http.ResponseWriter, _ *http.Request) {
	s.respond(w, s.service.Accounts(), nil)
}

func (s *Server) handleAccountAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Alias    string          `json:"alias"`
		Platform driver.Platform `json:"platform"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	item, err := s.service.AddAccount(body.Alias, body.Platform)
	s.respond(w, item, err)
}

func (s *Server) handleAccountStatus(w http.ResponseWriter, r *http.Request) {
	var body accountRef
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	status, err := s.service.Status(r.Context(), body.Account)
	s.respond(w, status, err)
}

func (s *Server) handleAccountActivate(w http.ResponseWriter, r *http.Request) {
	var body accountRef
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	item, err := s.service.Activate(r.Context(), body.Account)
	s.respond(w, item, err)
}

func (s *Server) handleAccountDeactivate(w http.ResponseWriter, r *http.Request) {
	var body accountRef
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	item, err := s.service.Deactivate(r.Context(), body.Account)
	s.respond(w, item, err)
}

func (s *Server) handleAccountRemove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account   string `json:"account"`
		Purge     bool   `json:"purge"`
		Confirmed bool   `json:"confirmed"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	item, err := s.service.RemoveAccount(r.Context(), body.Account, body.Purge, body.Confirmed)
	s.respond(w, item, err)
}

func (s *Server) handleAuthBegin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account    string `json:"account"`
		LAN        bool   `json:"lan"`
		LANAddress string `json:"lan_address,omitempty"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	challenge, err := s.service.BeginAuth(r.Context(), body.Account, body.LAN, body.LANAddress)
	s.respond(w, challenge, err)
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	challenge, err := s.service.AuthStatus(body.ID)
	s.respond(w, challenge, err)
}

func (s *Server) handleAuthSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	err := s.service.SubmitAuthCode(r.Context(), body.ID, body.Code)
	s.respond(w, map[string]bool{"accepted": err == nil}, err)
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	var body accountRef
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	capabilities, err := s.service.Capabilities(r.Context(), body.Account)
	s.respond(w, capabilities, err)
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
		Search  string `json:"search,omitempty"`
		Unread  bool   `json:"unread,omitempty"`
		Limit   int    `json:"limit,omitempty"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	items, err := s.service.ListConversations(r.Context(), body.Account, body.Search, body.Unread, body.Limit)
	s.respond(w, items, err)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account        string `json:"account"`
		ConversationID string `json:"conversation_id,omitempty"`
		AfterSequence  int64  `json:"after_sequence,omitempty"`
		Limit          int    `json:"limit,omitempty"`
		Latest         bool   `json:"latest,omitempty"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	items, err := s.service.ListMessages(r.Context(), body.Account, driver.MessageQuery{ConversationID: body.ConversationID, AfterSequence: body.AfterSequence, Limit: body.Limit, Latest: body.Latest})
	s.respond(w, items, err)
}

func (s *Server) handleMessagesWatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account       string `json:"account"`
		AfterSequence int64  `json:"after_sequence,omitempty"`
		TimeoutMS     int    `json:"timeout_ms,omitempty"`
		Limit         int    `json:"limit,omitempty"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	items, err := s.service.PollMessages(r.Context(), body.Account, body.AfterSequence, time.Duration(body.TimeoutMS)*time.Millisecond, body.Limit)
	s.respond(w, items, err)
}

func (s *Server) handleMessagesSearch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
		Text    string `json:"text"`
		Limit   int    `json:"limit,omitempty"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	items, err := s.service.SearchMessages(r.Context(), body.Account, body.Text, body.Limit)
	s.respond(w, items, err)
}

func (s *Server) handlePrepareSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
		driver.SendRequest
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	prepared, err := s.service.PrepareSend(r.Context(), body.Account, body.SendRequest)
	s.respond(w, prepared, err)
}

func (s *Server) handleCommitSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TransactionID  string `json:"transaction_id"`
		IdempotencyKey string `json:"idempotency_key"`
		Confirmed      bool   `json:"confirmed"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	result, err := s.service.CommitSend(r.Context(), body.TransactionID, body.IdempotencyKey, body.Confirmed)
	s.respond(w, result, err)
}

func (s *Server) handleSurfaceOpen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account          string `json:"account"`
		Ref              string `json:"ref"`
		MiniProgram      string `json:"mini_program"`
		WithoutImageData bool   `json:"without_image_data,omitempty"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	ref := strings.TrimSpace(body.Ref)
	miniProgram := strings.TrimSpace(body.MiniProgram)
	if (ref == "") == (miniProgram == "") {
		s.respond(w, nil, api.NewError(http.StatusBadRequest, api.CodeInvalidArgument, "exactly one of ref or mini_program is required"))
		return
	}
	var surface driver.Surface
	var err error
	if miniProgram != "" {
		surface, err = s.service.OpenNamedSurface(r.Context(), body.Account, driver.NamedSurface{Kind: "miniprogram", Name: miniProgram})
	} else {
		surface, err = s.service.OpenSurface(r.Context(), body.Account, ref)
	}
	s.respond(w, surfacePayload(surface, !body.WithoutImageData), err)
}

func (s *Server) handleSurfaceSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account          string `json:"account"`
		ID               string `json:"id"`
		WithoutImageData bool   `json:"without_image_data,omitempty"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	surface, err := s.service.SnapshotSurface(r.Context(), body.Account, body.ID)
	s.respond(w, surfacePayload(surface, !body.WithoutImageData), err)
}

type surfaceActRequest struct {
	Account          string  `json:"account"`
	ID               string  `json:"id"`
	ActionID         string  `json:"action_id"`
	Text             *string `json:"text,omitempty"`
	Confirmed        bool    `json:"confirmed,omitempty"`
	WithoutImageData bool    `json:"without_image_data,omitempty"`
}

func (request surfaceActRequest) action() driver.SurfaceAction {
	action := driver.SurfaceAction{
		ActionID: request.ActionID, TextProvided: request.Text != nil, Confirmed: request.Confirmed,
	}
	if request.Text != nil {
		action.Text = *request.Text
	}
	return action
}

func (s *Server) handleSurfaceAct(w http.ResponseWriter, r *http.Request) {
	var body surfaceActRequest
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	surface, err := s.service.ActSurface(r.Context(), body.Account, body.ID, body.action())
	s.respond(w, surfacePayload(surface, !body.WithoutImageData), err)
}

func (s *Server) handleSurfaceExport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account    string `json:"account"`
		ID         string `json:"id"`
		AssetToken string `json:"asset_token"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	result, err := s.service.ExportSurfaceAsset(r.Context(), body.Account, body.ID, body.AssetToken)
	s.respond(w, surfaceExportPayload(result), err)
}

func (s *Server) handleSurfaceClose(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
		ID      string `json:"id"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		s.respond(w, nil, err)
		return
	}
	err := s.service.CloseSurface(r.Context(), body.Account, body.ID)
	s.respond(w, map[string]bool{"closed": err == nil}, err)
}

func (s *Server) respond(w http.ResponseWriter, data any, err error) {
	if err != nil {
		response, status := api.Failure(err)
		api.WriteJSON(w, status, response)
		return
	}
	api.WriteJSON(w, http.StatusOK, api.Success(data))
}

type accountRef struct {
	Account string `json:"account"`
}

func surfacePayload(surface driver.Surface, includeImageData bool) map[string]any {
	result := map[string]any{"surface": surface}
	if includeImageData {
		result["screenshot_base64"] = base64.StdEncoding.EncodeToString(surface.Screenshot)
	}
	return result
}

func surfaceExportPayload(result driver.SurfaceAssetExport) map[string]any {
	return map[string]any{
		"asset":       result,
		"data_base64": base64.StdEncoding.EncodeToString(result.Data),
	}
}
