package wechat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientInstallerRequiresAndVerifiesChecksum(t *testing.T) {
	payload := []byte("synthetic AppImage fixture")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = writer.Write(payload)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	destination := filepath.Join(t.TempDir(), "clients", "WeChat.AppImage")
	result, err := (ClientInstaller{}).Download(context.Background(), InstallRequest{
		URL: server.URL + "/WeChat.AppImage", SHA256: hex.EncodeToString(digest[:]),
		Destination: destination, AllowedHosts: []string{host}, AllowHTTPForTest: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA256 != hex.EncodeToString(digest[:]) || result.Size != int64(len(payload)) {
		t.Fatalf("unexpected install result: %+v", result)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != string(payload) {
		t.Fatalf("installed payload mismatch: data=%q err=%v", data, err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("installed client mode = %o, want 700", info.Mode().Perm())
	}
}

func TestClientInstallerDeletesChecksumMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("wrong"))
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	host, _, _ := net.SplitHostPort(parsed.Host)
	destination := filepath.Join(t.TempDir(), "clients", "WeChat.AppImage")
	_, err := (ClientInstaller{}).Download(context.Background(), InstallRequest{
		URL: server.URL, SHA256: strings.Repeat("0", 64), Destination: destination,
		AllowedHosts: []string{host}, AllowHTTPForTest: true,
	})
	if err == nil {
		t.Fatal("expected checksum mismatch")
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("checksum mismatch left a destination file: %v", statErr)
	}
}
