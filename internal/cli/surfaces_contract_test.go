package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"golang.org/x/sys/unix"
)

func TestSurfaceScreenshotOutputIsReservedBeforeAnyDaemonRequest(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown fixture daemon: %v", err)
		}
		if err := <-serveDone; err != nil && err != http.ErrServerClosed {
			t.Errorf("serve fixture daemon: %v", err)
		}
	})

	existing := filepath.Join(t.TempDir(), "existing.png")
	if err := os.WriteFile(existing, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
	}{
		{name: "open", args: []string{"surfaces", "open", "--account", "personal", "--ref", "surface-ref"}},
		{name: "snapshot", args: []string{"surfaces", "snapshot", "--account", "personal", "--surface", "surface-1"}},
		{name: "act", args: []string{"surfaces", "act", "--account", "personal", "--surface", "surface-1", "--action", "action-1"}},
		{name: "back", args: []string{"surfaces", "back", "--account", "personal", "--surface", "surface-1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			root := NewRoot("test", bytes.NewReader(nil), &stdout, &bytes.Buffer{})
			args := append([]string{"--socket", socket, "--json"}, test.args...)
			root.SetArgs(append(args, "--screenshot-out", existing))
			err := root.ExecuteContext(context.Background())
			var appErr *api.AppError
			if !errors.As(err, &appErr) || appErr.Code != api.CodeInvalidArgument {
				t.Fatalf("surface command error = %v, want INVALID_ARGUMENT", err)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("daemon received %d requests before output reservation failed", got)
			}
		})
	}
	sharedDirectory := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(sharedDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sharedDirectory, 0o777); err != nil {
		t.Fatal(err)
	}
	root := NewRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{
		"--socket", socket, "--json", "surfaces", "act",
		"--account", "personal", "--surface", "surface-1", "--action", "action-1",
		"--screenshot-out", filepath.Join(sharedDirectory, "snapshot.png"),
	})
	var appErr *api.AppError
	if err := root.ExecuteContext(context.Background()); !errors.As(err, &appErr) || appErr.Code != api.CodeInvalidArgument {
		t.Fatalf("shared output directory error = %v, want INVALID_ARGUMENT", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("daemon received %d requests before shared output directory was rejected", got)
	}
	sharedAncestor := filepath.Join(t.TempDir(), "shared-ancestor")
	privateChild := filepath.Join(sharedAncestor, "private-child")
	if err := os.MkdirAll(privateChild, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sharedAncestor, 0o777); err != nil {
		t.Fatal(err)
	}
	root = NewRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{
		"--socket", socket, "--json", "surfaces", "act",
		"--account", "personal", "--surface", "surface-1", "--action", "action-1",
		"--screenshot-out", filepath.Join(privateChild, "snapshot.png"),
	})
	if err := root.ExecuteContext(context.Background()); !errors.As(err, &appErr) || appErr.Code != api.CodeInvalidArgument {
		t.Fatalf("shared output ancestor error = %v, want INVALID_ARGUMENT", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("daemon received %d requests before shared output ancestry was rejected", got)
	}
}

func TestSurfaceActCleansReservedOutputWhenScreenshotValidationFails(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	outputPath := filepath.Join(t.TempDir(), "snapshot.png")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	reservationObserved := make(chan bool, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/surfaces/act" {
			http.NotFound(w, request)
			return
		}
		info, statErr := os.Lstat(outputPath)
		reservationObserved <- statErr == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o600
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Success(surfaceOutput{Surface: driver.Surface{
			ID: "surface-1", Kind: "miniprogram", ScreenshotSHA256: "not-a-matching-digest",
			ObservedAt: time.Now().UTC(),
		}, ScreenshotBase64: "cG5n"}))
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown fixture daemon: %v", err)
		}
		if err := <-serveDone; err != nil && err != http.ErrServerClosed {
			t.Errorf("serve fixture daemon: %v", err)
		}
	})

	root := NewRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{
		"--socket", socket, "--json", "surfaces", "act",
		"--account", "personal", "--surface", "surface-1", "--action", "action-1",
		"--screenshot-out", outputPath,
	})
	if err := root.ExecuteContext(context.Background()); err == nil {
		t.Fatal("surface act accepted a mismatched screenshot")
	}
	if !<-reservationObserved {
		t.Fatal("daemon request arrived before a private output file was reserved")
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed screenshot output was not precisely removed: %v", err)
	}
}

func TestFinalizeSurfaceOutputVerifiesInlineScreenshot(t *testing.T) {
	png := []byte("bounded-png-payload")
	digest := sha256.Sum256(png)
	valid := surfaceOutput{
		Surface:          driver.Surface{ScreenshotSHA256: hex.EncodeToString(digest[:])},
		ScreenshotBase64: base64.StdEncoding.EncodeToString(png),
	}
	if err := finalizeSurfaceOutput(&valid, nil, false); err != nil {
		t.Fatalf("verify inline screenshot: %v", err)
	}
	if got := valid.ScreenshotBase64; got != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("inline screenshot changed after verification: %q", got)
	}

	invalid := valid
	invalid.Surface.ScreenshotSHA256 = strings.Repeat("0", 64)
	if err := finalizeSurfaceOutput(&invalid, nil, false); err == nil {
		t.Fatal("inline screenshot with a mismatched digest was accepted")
	}
}

func TestPrivateOutputTraversalRejectsAnAncestorMoveAfterOpeningTheLeaf(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	victim := filepath.Join(shared, "private")
	safeParent := filepath.Join(base, "safe")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(safeParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(safeParent, "moved-private")
	var hookErr error
	hookRan := false
	fd, err := openPrivateOutputDirectoryWithHook(victim, func(opened string) {
		if hookRan || opened != victim {
			return
		}
		hookRan = true
		if renameErr := os.Rename(victim, moved); renameErr != nil {
			hookErr = renameErr
			return
		}
		hookErr = os.Mkdir(victim, 0o700)
	})
	if fd >= 0 {
		_ = unix.Close(fd)
	}
	if hookErr != nil {
		t.Fatalf("exchange output ancestor fixture: %v", hookErr)
	}
	if !hookRan {
		t.Fatal("output traversal hook did not observe the original leaf")
	}
	if err == nil {
		t.Fatal("output traversal accepted a leaf opened through a non-sticky writable ancestor")
	}
}

func TestSurfaceActPreservesTextPresenceAndBackOmitsText(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	requestBody := make(chan map[string]any, 1)
	screenshot := []byte("fixture-screenshot")
	screenshotDigest := sha256.Sum256(screenshot)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			http.NotFound(w, request)
			return
		}
		if request.URL.Path == "/v1/surfaces/snapshot" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(api.Success(surfaceOutput{Surface: driver.Surface{
				ID: "surface-1", Kind: "miniprogram", ObservedAt: time.Now().UTC(),
				Actions: []driver.Action{{ID: "back-action", Label: "Back", Kind: "back", Risk: "low"}},
			}}))
			return
		}
		if request.URL.Path != "/v1/surfaces/act" {
			http.NotFound(w, request)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requestBody <- body
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Success(surfaceOutput{
			Surface: driver.Surface{
				ID: "surface-1", Kind: "miniprogram", ObservedAt: time.Now().UTC(),
				ScreenshotSHA256: hex.EncodeToString(screenshotDigest[:]),
			},
			ScreenshotBase64: base64.StdEncoding.EncodeToString(screenshot),
		}))
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown fixture daemon: %v", err)
		}
		if err := <-serveDone; err != nil && err != http.ErrServerClosed {
			t.Errorf("serve fixture daemon: %v", err)
		}
	})

	run := func(t *testing.T, args ...string) map[string]any {
		t.Helper()
		var stdout bytes.Buffer
		root := NewRoot("test", bytes.NewReader(nil), &stdout, &bytes.Buffer{})
		root.SetArgs(append([]string{"--socket", socket, "--json"}, args...))
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("surface command: %v", err)
		}
		return <-requestBody
	}

	body := run(t, "surfaces", "act",
		"--account", "personal", "--surface", "surface-1", "--action", "canvas-input",
		"--text", "宿舍", "--confirm", "--without-image-data")
	if body["account"] != "personal" || body["id"] != "surface-1" || body["action_id"] != "canvas-input" ||
		body["text"] != "宿舍" || body["confirmed"] != true || body["without_image_data"] != true {
		t.Fatalf("surface act daemon body = %#v", body)
	}

	body = run(t, "surfaces", "act",
		"--account", "personal", "--surface", "surface-1", "--action", "canvas-input")
	if _, ok := body["text"]; ok {
		t.Fatalf("surface act without --text sent a text field: %#v", body)
	}

	body = run(t, "surfaces", "act",
		"--account", "personal", "--surface", "surface-1", "--action", "canvas-input", "--text", "")
	if text, ok := body["text"]; !ok || text != "" {
		t.Fatalf("surface act --text empty did not preserve explicit empty string: %#v", body)
	}

	body = run(t, "surfaces", "back",
		"--account", "personal", "--surface", "surface-1")
	if body["action_id"] != "back-action" {
		t.Fatalf("surface back action ID = %#v", body)
	}
	if _, ok := body["text"]; ok {
		t.Fatalf("surface back sent a text field: %#v", body)
	}
}
