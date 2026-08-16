package wecom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompanionClientAuthenticatesAndDecodesSnapshot(t *testing.T) {
	token := "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Fatalf("missing bearer token")
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(UISnapshot{
			Sequence: 7, PackageName: DefaultWeComPackage,
			WindowClass: "com.tencent.wework.login.controller.LoginWxAuthActivity",
			Nodes:       []Node{{ID: "0/1", Text: "Agree", VisibleToUser: true}},
		})
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Sequence != 7 || snapshot.PackageName != DefaultWeComPackage ||
		snapshot.WindowClass != "com.tencent.wework.login.controller.LoginWxAuthActivity" ||
		len(snapshot.Nodes) != 1 || !snapshot.Nodes[0].VisibleToUser {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestCompanionActionValidation(t *testing.T) {
	invalid := []CompanionAction{
		{Kind: ActionClick},
		{Kind: ActionGlobalBack, Text: "unexpected"},
		{Kind: "shell", Text: "id"},
	}
	for _, action := range invalid {
		if err := validateCompanionAction(action); err == nil {
			t.Fatalf("expected action to be rejected: %+v", action)
		}
	}
}

func TestCompanionClientRejectsNonLoopback(t *testing.T) {
	_, err := newCompanionClientForURL("http://192.0.2.1:18765", "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789", nil)
	if err == nil {
		t.Fatal("expected non-loopback companion URL to be rejected")
	}
}
