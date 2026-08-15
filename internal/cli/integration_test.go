package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/account"
	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/daemon"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/driver/fake"
	"github.com/gih10012/wechatcopilot/internal/service"
)

func TestCLIThroughUnixDaemon(t *testing.T) {
	paths, shutdown := startFakeDaemon(t)
	t.Cleanup(shutdown)

	var added account.Account
	runCLI(t, paths.Socket, []string{"accounts", "add", "--alias", "personal", "--platform", "wechat"}, &added)
	if added.ID == "" {
		t.Fatal("accounts add returned no ID")
	}
	var activated account.Account
	runCLI(t, paths.Socket, []string{"accounts", "activate", "--account", added.ID}, &activated)
	if !activated.Active {
		t.Fatalf("account was not active: %#v", activated)
	}

	var conversations []driver.Conversation
	runCLI(t, paths.Socket, []string{"conversations", "list", "--account", added.ID}, &conversations)
	if len(conversations) != 1 {
		t.Fatalf("unexpected conversations: %#v", conversations)
	}
	var prepared struct {
		ID string `json:"id"`
	}
	runCLI(t, paths.Socket, []string{
		"messages", "prepare-send", "--account", added.ID,
		"--conversation", conversations[0].ID, "--text", "hello",
	}, &prepared)
	if prepared.ID == "" {
		t.Fatal("prepare-send returned no transaction ID")
	}
	var sent driver.SendResult
	runCLI(t, paths.Socket, []string{
		"messages", "commit-send", "--transaction", prepared.ID,
		"--idempotency-key", "cli-test-1", "--confirm",
	}, &sent)
	if !sent.Verified {
		t.Fatalf("send was not verified: %#v", sent)
	}
	var latest []driver.Message
	runCLI(t, paths.Socket, []string{
		"messages", "history", "--account", added.ID,
		"--conversation", conversations[0].ID, "--latest", "--limit", "1",
	}, &latest)
	if len(latest) != 1 || latest[0].ID != sent.MessageID {
		t.Fatalf("latest history did not return the newest message: %#v", latest)
	}

	var surface surfaceOutput
	runCLI(t, paths.Socket, []string{"surfaces", "open", "--account", added.ID, "--ref", "fixture"}, &surface)
	if surface.Surface.ID == "" {
		t.Fatalf("surface open returned no ID: %#v", surface)
	}
	var closed map[string]bool
	runCLI(t, paths.Socket, []string{"surfaces", "close", "--account", added.ID, "--surface", surface.Surface.ID}, &closed)
	if !closed["closed"] {
		t.Fatalf("surface was not closed: %#v", closed)
	}

	runCLI(t, paths.Socket, []string{"accounts", "deactivate", "--account", added.ID}, &account.Account{})
	runCLI(t, paths.Socket, []string{"accounts", "remove", "--account", added.ID, "--purge", "--confirm"}, &account.Account{})
	var accounts []account.Account
	runCLI(t, paths.Socket, []string{"accounts", "list"}, &accounts)
	if len(accounts) != 0 {
		t.Fatalf("removed account remains: %#v", accounts)
	}
}

func runCLI(t *testing.T, socket string, args []string, output any) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	root := NewRoot("test", bytes.NewReader(nil), &stdout, &stderr)
	root.SetArgs(append([]string{"--socket", socket, "--json"}, args...))
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("wechatcopilot %v: %v stderr=%s stdout=%s", args, err, stderr.String(), stdout.String())
	}
	var envelope struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode CLI output for %v: %v: %s", args, err, stdout.String())
	}
	if !envelope.OK {
		t.Fatalf("CLI returned failure for %v: %s", args, stdout.String())
	}
	if err := json.Unmarshal(envelope.Data, output); err != nil {
		t.Fatalf("decode CLI data for %v: %v: %s", args, err, envelope.Data)
	}
}

func startFakeDaemon(t *testing.T) (config.Paths, func()) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "state")
	runtimeDir := filepath.Join(root, "runtime")
	paths := config.Paths{
		Home: home, Accounts: filepath.Join(home, "accounts"), Downloads: filepath.Join(home, "downloads"),
		Runtime: runtimeDir, Socket: filepath.Join(runtimeDir, "daemon.sock"), Registry: filepath.Join(home, "accounts.json"),
	}
	factories := map[driver.Platform]driver.Factory{
		driver.PlatformWeChat: func(driver.AccountRuntime) (driver.Driver, error) { return fake.New(driver.PlatformWeChat), nil },
		driver.PlatformWeCom:  func(driver.AccountRuntime) (driver.Driver, error) { return fake.New(driver.PlatformWeCom), nil },
	}
	control, err := service.New(paths, factories)
	if err != nil {
		t.Fatal(err)
	}
	server := daemon.New(paths.Socket, control)
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown daemon: %v", err)
		}
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("serve daemon: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	}
	return paths, shutdown
}
