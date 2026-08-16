package wecom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	core "github.com/gih10012/wechatcopilot/internal/driver"
)

func TestSubmitAuthCodeRevalidatesBeforeClicking(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	var mu sync.Mutex
	phase := 0
	actions := make([]CompanionAction, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot":
			mu.Lock()
			currentPhase := phase
			mu.Unlock()
			snapshot := UISnapshot{
				Sequence:    40 + int64(currentPhase),
				PackageName: DefaultWeComPackage,
				WindowClass: weComSMSVerifyActivity,
				Nodes: []Node{
					{ID: "0/marker", Text: "Verification code", Enabled: true, VisibleToUser: true},
					{ID: "0/code", Editable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 100, Right: 300, Bottom: 160}},
				},
			}
			if currentPhase > 0 {
				snapshot.Nodes = append(snapshot.Nodes, Node{
					ID: "0/submit", Text: "Continue", Clickable: true, Enabled: true,
					VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 180, Right: 300, Bottom: 240},
				})
			}
			_ = json.NewEncoder(writer).Encode(snapshot)
		case request.Method == http.MethodPost && request.URL.Path == "/v1/actions":
			var action CompanionAction
			if err := json.NewDecoder(request.Body).Decode(&action); err != nil {
				http.Error(writer, "bad action", http.StatusBadRequest)
				return
			}
			mu.Lock()
			actions = append(actions, action)
			if action.Kind == ActionSetText {
				phase = 1
			}
			currentPhase := phase
			mu.Unlock()
			_ = json.NewEncoder(writer).Encode(ActionResult{Accepted: true, Sequence: 40 + int64(currentPhase)})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		config: DefaultConfig(), companion: client, running: true,
		android: AndroidContainer{
			DockerBinary: "docker", Container: "synthetic",
			Executor: functionExecutor(func(context.Context, string, ...string) ([]byte, error) {
				return []byte("topResumedActivity=ActivityRecord{x u0 " + DefaultWeComPackage + "/.login.controller.LoginVeryfyStep2Activity}\n"), nil
			}),
			Verify: func(context.Context) error { return nil },
		},
	}
	driverInstance := &Driver{
		runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := driverInstance.SubmitAuthCode(ctx, "123456"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(actions) != 2 {
		t.Fatalf("SMS submission actions = %#v", actions)
	}
	if actions[0].Kind != ActionSetText || actions[0].NodeID != "0/code" ||
		actions[0].Text != "123456" || actions[0].ExpectedSequence != 40 {
		t.Fatalf("SMS set-text action = %#v", actions[0])
	}
	if actions[1].Kind != ActionClick || actions[1].NodeID != "0/submit" ||
		actions[1].Text != "" || actions[1].ExpectedSequence != 41 {
		t.Fatalf("SMS submit action = %#v", actions[1])
	}
	if state, _ := classifyLogin(UISnapshot{PackageName: DefaultWeComPackage, WindowClass: weComSMSVerifyActivity}); state != core.StateAuthRequired {
		t.Fatalf("verified SMS activity state = %s", state)
	}
}
