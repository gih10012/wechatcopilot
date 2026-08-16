package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
			Nodes: []Node{{
				ID: "0/1", Text: "Agree", Checkable: true, Checked: true,
				Enabled: true, VisibleToUser: true,
			}},
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
		len(snapshot.Nodes) != 1 || !snapshot.Nodes[0].Checkable || !snapshot.Nodes[0].Checked ||
		!snapshot.Nodes[0].Enabled || !snapshot.Nodes[0].VisibleToUser {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestCompanionNodeCheckboxFieldsUseWireNamesAndDefaultFailClosed(t *testing.T) {
	var checked Node
	if err := json.Unmarshal([]byte(`{"id":"0/1","checkable":true,"checked":true}`), &checked); err != nil {
		t.Fatal(err)
	}
	if !checked.Checkable || !checked.Checked {
		t.Fatalf("checkbox wire fields were not decoded: %+v", checked)
	}

	var omitted Node
	if err := json.Unmarshal([]byte(`{"id":"0/1","enabled":true,"visible_to_user":true}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if omitted.Checkable || omitted.Checked {
		t.Fatalf("missing checkbox fields must remain false: %+v", omitted)
	}
}

func TestCompanionActionValidation(t *testing.T) {
	if err := validateCompanionAction(CompanionAction{
		Kind: ActionCheck, NodeID: "0/1", ExpectedSequence: 7,
	}); err != nil {
		t.Fatalf("expected constrained check action to be accepted: %v", err)
	}

	invalid := []CompanionAction{
		{Kind: ActionClick},
		{Kind: ActionCheck},
		{Kind: ActionCheck, NodeID: "0/1", ExpectedSequence: 7, Text: "unexpected"},
		{Kind: ActionGlobalBack, Text: "unexpected"},
		{Kind: "shell", Text: "id"},
	}
	for _, action := range invalid {
		if err := validateCompanionAction(action); err == nil {
			t.Fatalf("expected action to be rejected: %+v", action)
		}
	}
}

func TestCompanionActionResponseLossAfterDispatchIsUncertain(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	received := make(chan CompanionAction, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var action CompanionAction
		if err := json.NewDecoder(request.Body).Decode(&action); err != nil {
			http.Error(writer, "bad action", http.StatusBadRequest)
			return
		}
		received <- action
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack action response: %v", err)
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	action := CompanionAction{Kind: ActionClick, NodeID: "0/1", ExpectedSequence: 7}
	result, err := client.Act(context.Background(), action)
	if !errors.Is(err, ErrActionOutcomeUncertain) {
		t.Fatalf("response-loss error = %v, want ErrActionOutcomeUncertain", err)
	}
	if result != (ActionResult{}) {
		t.Fatalf("uncertain action result = %#v, want zero value", result)
	}
	select {
	case got := <-received:
		if got != action {
			t.Fatalf("received action = %#v, want %#v", got, action)
		}
	default:
		t.Fatal("handler did not receive action before disconnecting")
	}
}

func TestCompanionActionRequiresExplicitDecodableAcceptance(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "HTTP failure after dispatch", status: http.StatusInternalServerError, body: `{"error":"outcome uncertain"}`},
		{name: "malformed JSON", status: http.StatusOK, body: `{"accepted":`},
		{name: "missing accepted", status: http.StatusOK, body: `{"sequence":8}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := newCompanionClientForURL(server.URL, token, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Act(context.Background(), CompanionAction{
				Kind: ActionClick, NodeID: "0/1", ExpectedSequence: 7,
			})
			if !errors.Is(err, ErrActionOutcomeUncertain) {
				t.Fatalf("response error = %v, want ErrActionOutcomeUncertain", err)
			}
		})
	}
}

func TestCompanionActionExplicitRejectionIsNotUncertain(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(ActionResult{Accepted: false, Sequence: 8, Detail: "stale node"})
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Act(context.Background(), CompanionAction{
		Kind: ActionClick, NodeID: "0/1", ExpectedSequence: 7,
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("explicit rejection error = %v, want ErrStale", err)
	}
	if errors.Is(err, ErrActionOutcomeUncertain) {
		t.Fatalf("explicit rejection was marked uncertain: %v", err)
	}
	if result.Accepted || result.Sequence != 8 {
		t.Fatalf("explicit rejection result = %#v", result)
	}
}

func TestContainerCompanionActionTransportFailureAfterDispatchIsUncertain(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	android := AndroidContainer{
		DockerBinary: "docker", Container: "synthetic",
		Executor: functionExecutor(func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("response channel closed")
		}),
		Verify: func(context.Context) error { return nil },
	}
	client, err := newContainerCompanionClient(android, DefaultCompanionPort, token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Act(context.Background(), CompanionAction{
		Kind: ActionClick, NodeID: "0/1", ExpectedSequence: 7,
	})
	if !errors.Is(err, ErrActionOutcomeUncertain) {
		t.Fatalf("container action error = %v, want ErrActionOutcomeUncertain", err)
	}
}

func TestCompanionClientRejectsNonLoopback(t *testing.T) {
	_, err := newCompanionClientForURL("http://192.0.2.1:18765", "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789", nil)
	if err == nil {
		t.Fatal("expected non-loopback companion URL to be rejected")
	}
}
