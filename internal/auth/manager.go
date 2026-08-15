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
	mu       sync.Mutex
	public   Challenge
	token    string
	driver   driver.Driver
	server   *http.Server
	listener net.Listener
	attempts int
	closed   bool
	done     chan struct{}
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
	item := &entry{public: public, token: token, driver: instance, listener: listener, done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc(path, item.handlePage)
	mux.HandleFunc(path+"/state", item.handleState)
	mux.HandleFunc(path+"/image", item.handleImage)
	mux.HandleFunc(path+"/submit", item.handleSubmit)
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
	if time.Now().UTC().After(item.public.ExpiresAt) && item.public.CompletedAt == nil {
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
		item.mu.Lock()
		if time.Now().UTC().After(item.public.ExpiresAt) {
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
		item.mu.Lock()
		item.public.State = snapshot.State
		item.public.Kind = snapshot.Kind
		item.public.Prompt = snapshot.Prompt
		if snapshot.State == driver.StateOnline {
			now := time.Now().UTC()
			item.public.CompletedAt = &now
			item.mu.Unlock()
			timer := time.NewTimer(completedRetention)
			select {
			case <-item.done:
				timer.Stop()
			case <-timer.C:
			}
			return
		}
		item.mu.Unlock()
	}
}

func (e *entry) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	e.mu.Lock()
	if time.Now().UTC().After(e.public.ExpiresAt) {
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
	e.mu.Lock()
	expired := time.Now().UTC().After(e.public.ExpiresAt)
	e.mu.Unlock()
	if expired {
		http.Error(w, "authentication challenge expired", http.StatusGone)
		return
	}
	snapshot, err := e.driver.AuthSnapshot(r.Context())
	if err != nil {
		http.Error(w, "driver unavailable", http.StatusServiceUnavailable)
		return
	}
	e.mu.Lock()
	e.public.State = snapshot.State
	e.public.Kind = snapshot.Kind
	e.public.Prompt = snapshot.Prompt
	data := e.public
	e.mu.Unlock()
	writeJSON(w, data)
}

func (e *entry) handleImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	e.mu.Lock()
	unavailable := time.Now().UTC().After(e.public.ExpiresAt) || e.public.CompletedAt != nil || e.public.State == driver.StateOnline
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
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "invalid verification code", http.StatusBadRequest)
		return
	}
	if err := e.submitCode(r.Context(), body.Code); err != nil {
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

var errTooManyAttempts = errors.New("too many verification attempts")

func (e *entry) submitCode(ctx context.Context, code string) error {
	if !verificationCodePattern.MatchString(code) {
		return errors.New("invalid verification code")
	}
	e.mu.Lock()
	if e.closed || time.Now().UTC().After(e.public.ExpiresAt) || e.public.CompletedAt != nil || e.public.State == driver.StateOnline {
		e.mu.Unlock()
		return os.ErrNotExist
	}
	e.attempts++
	attempts := e.attempts
	e.mu.Unlock()
	if attempts > 8 {
		return errTooManyAttempts
	}
	return e.driver.SubmitAuthCode(ctx, code)
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
:root{color-scheme:light;background:#f2efe8;color:#17211d;font-family:ui-sans-serif,system-ui,sans-serif}body{margin:0;display:grid;min-height:100vh;place-items:center}.card{width:min(92vw,520px);background:#fff;border:1px solid #d7d1c5;border-radius:20px;padding:28px;box-shadow:0 20px 60px #263a3020}h1{margin:0 0 8px;font-size:26px}p{color:#59645f}.screen{display:block;width:min(100%,420px);max-height:460px;object-fit:contain;margin:20px auto;border-radius:12px;background:#e7e4dc}form{display:flex;gap:10px}input,button{font:inherit;padding:12px;border-radius:10px;border:1px solid #b8beb9}input{flex:1}button{background:#087f5b;color:#fff;border:0;font-weight:700}.meta{font-family:ui-monospace,monospace;font-size:12px}</style></head>
<body><main class="card"><h1>完成账号登录</h1><p id="prompt">{{.Prompt}}</p><img class="screen" src="{{.BasePath}}/image" alt="Login QR or client screen" onerror="this.style.display='none'">
<form id="code"><input name="code" inputmode="numeric" autocomplete="one-time-code" placeholder="手机验证码"><button>提交</button></form><p class="meta">Challenge {{.ID}} · 页面只在当前登录挑战期间有效</p></main>
<script>const base=location.pathname;const prompt=document.querySelector('#prompt');const form=document.querySelector('#code');const poll=setInterval(async()=>{const r=await fetch(base+'/state',{cache:'no-store'});if(r.ok){const x=await r.json();prompt.textContent=x.prompt||x.state;if(x.state==='ONLINE'){clearInterval(poll);prompt.textContent='登录完成，可以关闭此页面';form.hidden=true}}},1500);form.addEventListener('submit',async e=>{e.preventDefault();const code=new FormData(e.target).get('code');const r=await fetch(base+'/submit',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({code})});prompt.textContent=r.ok?'验证码已提交，请在手机上完成确认':'验证码无效或已过期'});</script></body></html>`))
