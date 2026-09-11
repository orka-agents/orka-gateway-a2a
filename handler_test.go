// Copyright (c) 2026. MIT License - see LICENSE file for details.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// Literal encoding of gev-1 and 2026-01-02T03:04:05.123456789Z, independent of
// the production encoder so the wire identity contract is pinned by the tests.
const fixtureTaskID = "a2a1.Z2V2LTE.MjAyNi0wMS0wMlQwMzowNDowNS4xMjM0NTY3ODla"

// The only fixture boundary is Orka's HTTP API. SDK discovery, encoding,
// transport, adapter auth, admission, polling and result mapping are real.
type ledgerFixture struct {
	mu              sync.Mutex
	event, delivery map[string]any
	envelopes       []map[string]any
	reads, creates  int
	finishAfter     int
	override        func(http.ResponseWriter, *http.Request) bool
}

func newLedger() *ledgerFixture {
	return &ledgerFixture{event: map[string]any{
		"id": "gev-1", "createdAt": "2026-01-02T03:04:05.123456789Z", "namespace": "demo", "namespaceUid": "ns-uid",
		"gatewayName": "a2a", "gatewayUid": "gw-uid", "gatewayGeneration": 1,
		"bindingName": "assistant", "bindingUid": "binding-uid", "bindingGeneration": 1,
		"agentName": "assistant", "agentUid": "agent-uid",
		"protocolVersion": "orka.gateway.v1", "eventType": "text", "externalEventId": "external", "state": "Accepted",
		"accountId": "account", "contextId": "route", "senderId": "caller", "threadId": "thread", "replyTarget": "route",
		"taskName": "task-1", "sessionName": "session-1",
	}, delivery: map[string]any{
		"id": "gdl-1", "idempotencyId": "gdl-1", "namespace": "demo", "namespaceUid": "ns-uid",
		"gatewayName": "a2a", "gatewayUid": "gw-uid", "gatewayGeneration": 1,
		"bindingName": "assistant", "eventId": "gev-1", "taskName": "task-1", "sessionName": "session-1",
		"accountId": "account", "contextId": "route", "threadId": "thread", "replyTarget": "route",
		"kind": "final", "state": "Pending", "text": "The real gateway result.",
	}}
}

func (f *ledgerFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.override != nil && f.override(w, r) {
		return
	}
	if r.URL.Path == "/api/v1/gateways/demo/a2a/events" {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture-inbound" {
			http.Error(w, "bad ingress credential or method", http.StatusUnauthorized)
			return
		}
		var env map[string]any
		if json.NewDecoder(r.Body).Decode(&env) != nil {
			http.Error(w, "bad JSON", 400)
			return
		}
		f.envelopes = append(f.envelopes, env)
		if len(f.envelopes) > 1 && !reflect.DeepEqual(f.envelopes[0], env) {
			http.Error(w, "private upstream diagnostic", http.StatusConflict)
			return
		}
		if f.creates == 0 {
			f.creates++
			f.event["threadId"] = env["threadId"]
			f.event["externalEventId"] = env["externalEventId"]
			f.delivery["threadId"] = env["threadId"]
		}
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "accepted", "eventId": "gev-1", "state": f.event["state"]})
		return
	}
	if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-read" ||
		r.URL.Query().Get("namespace") != "demo" {
		http.Error(w, "bad read credential, namespace or method", http.StatusUnauthorized)
		return
	}
	var result any
	switch r.URL.Path {
	case "/api/v1/agents/assistant":
		result = map[string]any{
			"metadata": map[string]any{"name": "assistant", "namespace": "demo", "uid": "agent-uid"},
			"spec":     map[string]any{"systemPrompt": "PRIVATE PROMPT NEVER PUBLISH"},
		}
	case "/api/v1/gateways/a2a":
		result = map[string]any{
			"metadata": map[string]any{"name": "a2a", "namespace": "demo", "uid": "gw-uid"},
			"spec":     map[string]any{"gatewayClassName": "a2a"},
		}
	case "/api/v1/gatewaybindings/assistant":
		result = map[string]any{
			"metadata": map[string]any{"name": "assistant", "namespace": "demo", "uid": "binding-uid"},
			"spec": map[string]any{
				"gatewayRef": map[string]string{"name": "a2a"}, "agentRef": map[string]string{"name": "assistant"},
				"match":        map[string]string{"accountId": "account", "contextId": "route", "senderId": "caller"},
				"senderPolicy": map[string]any{"mode": "allowlist", "allowedSenderIds": []string{"caller"}},
				"session":      map[string]string{"mode": "thread-sender"},
			},
		}
	case "/api/v1/gateway-events/gev-1":
		f.reads++
		if f.finishAfter > 0 && f.reads >= f.finishAfter {
			f.complete()
		}
		result = f.event
	case "/api/v1/gateway-deliveries/gdl-1":
		result = f.delivery
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}

func (f *ledgerFixture) complete() {
	f.event["state"] = "Completed"
	f.event["deliveryId"] = "gdl-1"
	f.event["taskUid"] = "task-uid"
}

func testConfig(t *testing.T) config {
	t.Helper()
	dir := t.TempDir()
	for name, value := range map[string]string{
		"client": "fixture-client", "inbound": "fixture-inbound",
		"outbound": "fixture-outbound", "read": "fixture-read",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return config{
		PublicURL: "https://agent.example", OrkaURL: "http://orka.invalid", Namespace: "demo",
		Gateway: "a2a", Binding: "assistant", Agent: "assistant",
		AccountID: "account", ContextID: "route", SenderID: "caller",
		ClientTokenFile: filepath.Join(dir, "client"), InboundTokenFile: filepath.Join(dir, "inbound"),
		OutboundTokenFile: filepath.Join(dir, "outbound"), ReadTokenFile: filepath.Join(dir, "read"),
		PollInterval: 5 * time.Millisecond, WaitTimeout: 150 * time.Millisecond,
		Card: cardDeclaration{
			Name: "Reviewed assistant", Description: "Public text assistant", Version: "1.0.0",
			Skills: []a2a.AgentSkill{{
				ID: "answer", Name: "Answer", Description: "Answer text questions", Tags: []string{"text"},
			}},
		},
	}
}

type authTransport struct {
	base  http.RoundTripper
	token string
}

func (a authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	c.Header.Set("Authorization", "Bearer "+a.token)
	return a.base.RoundTrip(c)
}

func startAdapter(t *testing.T, f *ledgerFixture) (*httptest.Server, *a2aclient.Client, config) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(up.Close)
	cfg := testConfig(t)
	cfg.OrkaURL = up.URL
	h, err := newAdapter(t.Context(), cfg, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	hc := s.Client()
	card, err := agentcard.NewResolver(hc).Resolve(t.Context(), s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].URL != "https://agent.example/a2a" ||
		card.SupportedInterfaces[0].ProtocolVersion != "1.0" {
		t.Fatalf("wrong reviewed endpoint: %+v", card)
	}
	if card.Name != "Reviewed assistant" || card.Capabilities.Streaming || card.Capabilities.PushNotifications ||
		card.Capabilities.ExtendedAgentCard || len(card.SecurityRequirements) != 1 {
		t.Fatalf("wrong card: %+v", card)
	}
	cardBytes, _ := json.Marshal(card)
	if bytes.Contains(cardBytes, []byte("PRIVATE")) || bytes.Contains(cardBytes, []byte("fixture-")) {
		t.Fatal("card leaked private data")
	}
	card.SupportedInterfaces[0].URL = s.URL + "/a2a"
	clientHTTP := &http.Client{Transport: authTransport{hc.Transport, "fixture-client"}, Timeout: time.Second}
	c, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithJSONRPCTransport(clientHTTP))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Destroy() })
	return s, c, cfg
}

func request(immediate bool) *a2a.SendMessageRequest {
	return &a2a.SendMessageRequest{
		Message: &a2a.Message{
			ID: "message-1", Role: a2a.MessageRoleUser, Parts: a2a.ContentParts{a2a.NewTextPart("Hello")},
		},
		Config: &a2a.SendMessageConfig{ReturnImmediately: immediate},
	}
}
func taskResult(t *testing.T, r a2a.SendMessageResult, err error) *a2a.Task {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	task, ok := r.(*a2a.Task)
	if !ok {
		t.Fatalf("not a Task: %T", r)
	}
	return task
}

func TestSDKAdmissionRetryAndDurableResult(t *testing.T) {
	f := newLedger()
	_, c, _ := startAdapter(t, f)
	r, err := c.SendMessage(t.Context(), request(true))
	first := taskResult(t, r, err)
	r, err = c.SendMessage(t.Context(), request(true))
	retry := taskResult(t, r, err)
	if first.ID != fixtureTaskID || first.Status.State != a2a.TaskStateSubmitted || first.ContextID == "" ||
		retry.ID != first.ID || retry.ContextID != first.ContextID {
		t.Fatalf("bad admission/retry: %+v %+v", first, retry)
	}
	f.mu.Lock()
	creates, reads, envelopes := f.creates, f.reads, append([]map[string]any(nil), f.envelopes...)
	f.complete()
	f.mu.Unlock()
	if creates != 1 || reads != 2 || len(envelopes) != 2 {
		t.Fatalf("admissions=%d reads=%d envelopes=%d", creates, reads, len(envelopes))
	}
	env := envelopes[0]
	for _, field := range []string{"occurredAt", "receivedAt", "metadata"} {
		if _, ok := env[field]; ok {
			t.Fatalf("unstable/private field %s", field)
		}
	}
	if env["protocolVersion"] != "orka.gateway.v1" || env["accountId"] != "account" || env["contextId"] != "route" ||
		env["eventType"] != "text" || env["text"] != "Hello" || env["sender"].(map[string]any)["id"] != "caller" {
		t.Fatalf("bad envelope: %+v", env)
	}
	final, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if final.Status.State != a2a.TaskStateCompleted || len(final.Artifacts) != 1 ||
		final.Artifacts[0].Parts[0].Text() != "The real gateway result." || len(final.History) != 0 {
		t.Fatalf("bad result: %+v", final)
	}
	again, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: first.ID})
	if err != nil || !reflect.DeepEqual(final, again) {
		t.Fatalf("unstable final: %v", err)
	}
	altered := request(true)
	altered.Message.Parts[0] = a2a.NewTextPart("Changed")
	_, err = c.SendMessage(t.Context(), altered)
	if !errors.Is(err, a2a.ErrInvalidParams) || strings.Contains(err.Error(), "private upstream") {
		t.Fatalf("altered replay: %v", err)
	}
}

func TestBlockingWaitsForResultAndTimeoutDoesNotCancel(t *testing.T) {
	t.Run("blocking default", func(t *testing.T) {
		f := newLedger()
		f.finishAfter = 3
		_, c, _ := startAdapter(t, f)
		q := request(false)
		q.Config = nil
		r, err := c.SendMessage(t.Context(), q)
		task := taskResult(t, r, err)
		if task.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("returned before result: %+v", task)
		}
	})
	t.Run("bounded wait", func(t *testing.T) {
		f := newLedger()
		_, c, _ := startAdapter(t, f)
		start := time.Now()
		_, err := c.SendMessage(t.Context(), request(false))
		if !errors.Is(err, a2a.ErrServerError) || time.Since(start) > time.Second {
			t.Fatalf("unbounded/false success: %v", err)
		}
		got, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID})
		if err != nil || got.Status.State != a2a.TaskStateSubmitted {
			t.Fatalf("timeout changed work: %+v %v", got, err)
		}
	})
	t.Run("disconnect", func(t *testing.T) {
		f := newLedger()
		_, c, _ := startAdapter(t, f)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		defer cancel()
		_, err := c.SendMessage(ctx, request(false))
		if err == nil {
			t.Fatal("expected canceled request")
		}
		time.Sleep(20 * time.Millisecond)
		f.mu.Lock()
		reads := f.reads
		f.mu.Unlock()
		time.Sleep(25 * time.Millisecond)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.reads != reads || f.creates != 1 || f.event["state"] != "Accepted" {
			t.Fatal("polling survived disconnect or work mutated")
		}
	})
}

func TestRejectUnsupportedInputsBeforeAdmission(t *testing.T) {
	cases := map[string]func(*a2a.SendMessageRequest){
		"file": func(q *a2a.SendMessageRequest) {
			q.Message.Parts = a2a.ContentParts{a2a.NewFileURLPart("https://example.org/private", "text/plain")}
		},
		"data": func(q *a2a.SendMessageRequest) {
			q.Message.Parts = a2a.ContentParts{a2a.NewDataPart(map[string]any{"namespace": "other"})}
		},
		"empty": func(q *a2a.SendMessageRequest) { q.Message.Parts = nil },
		"oversize": func(q *a2a.SendMessageRequest) {
			q.Message.Parts[0] = a2a.NewTextPart(strings.Repeat("x", 65537))
		},
		"missing ID":         func(q *a2a.SendMessageRequest) { q.Message.ID = "" },
		"role":               func(q *a2a.SendMessageRequest) { q.Message.Role = a2a.MessageRoleAgent },
		"continuation":       func(q *a2a.SendMessageRequest) { q.Message.TaskID = "gev-1" },
		"context whitespace": func(q *a2a.SendMessageRequest) { q.Message.ContextID = " context " },
		"tenant":             func(q *a2a.SendMessageRequest) { q.Tenant = "other" },
		"metadata":           func(q *a2a.SendMessageRequest) { q.Metadata = map[string]any{"sender": "other"} },
		"history":            func(q *a2a.SendMessageRequest) { n := -1; q.Config.HistoryLength = &n },
		"output":             func(q *a2a.SendMessageRequest) { q.Config.AcceptedOutputModes = []string{"image/png"} },
		"push":               func(q *a2a.SendMessageRequest) { q.Config.PushConfig = &a2a.PushConfig{} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newLedger()
			_, c, _ := startAdapter(t, f)
			q := request(true)
			change(q)
			if _, err := c.SendMessage(t.Context(), q); err == nil {
				t.Fatal("unsupported request admitted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.creates != 0 {
				t.Fatal("unsupported input created event")
			}
		})
	}
}

func TestOwnershipAndCancellation(t *testing.T) {
	for _, field := range []string{
		"namespace", "gatewayName", "gatewayUid", "bindingName", "bindingUid", "agentName", "agentUid",
		"accountId", "contextId", "senderId", "id",
	} {
		t.Run(field, func(t *testing.T) {
			f := newLedger()
			f.event[field] = "foreign"
			_, c, _ := startAdapter(t, f)
			if _, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID}); !errors.Is(err, a2a.ErrTaskNotFound) {
				t.Fatalf("foreign event revealed: %v", err)
			}
			_, err := c.CancelTask(t.Context(), &a2a.CancelTaskRequest{ID: fixtureTaskID})
			if !errors.Is(err, a2a.ErrTaskNotFound) {
				t.Fatalf("cancel revealed foreign event: %v", err)
			}
		})
	}
	f := newLedger()
	_, c, _ := startAdapter(t, f)
	_, err := c.CancelTask(t.Context(), &a2a.CancelTaskRequest{ID: fixtureTaskID})
	if !errors.Is(err, a2a.ErrTaskNotCancelable) {
		t.Fatalf("cancel policy: %v", err)
	}
	if _, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: "missing"}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("missing: %v", err)
	}
	n := -1
	_, err = c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID, HistoryLength: &n})
	if !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("history: %v", err)
	}
	if _, err := c.ListTasks(t.Context(), &a2a.ListTasksRequest{}); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Fatalf("list: %v", err)
	}
}

func TestStateMappingNeverInventsSuccess(t *testing.T) {
	cases := []struct {
		name, state, kind, deliveryState, text string
		want                                   a2a.TaskState
		wantErr                                bool
	}{
		{"queued", "Queued", "", "", "", a2a.TaskStateSubmitted, false},
		{"dispatching", "Dispatching", "", "", "", a2a.TaskStateWorking, false},
		{"running", "TaskCreated", "", "", "", a2a.TaskStateWorking, false},
		{"missing delivery", "Completed", "", "", "", "", true},
		{"empty output", "Completed", "final", "Pending", "", "", true},
		{"final", "Completed", "final", "Pending", "answer", a2a.TaskStateCompleted, false},
		{"error", "Completed", "error", "Pending", "failed", a2a.TaskStateFailed, false},
		{"rejected", "Rejected", "", "", "", a2a.TaskStateRejected, false},
		{"expired", "Expired", "", "", "", a2a.TaskStateFailed, false},
		{"dead letter", "DeadLettered", "", "", "", a2a.TaskStateFailed, false},
		{"unknown", "Surprise", "", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLedger()
			f.event["state"] = tc.state
			if tc.kind != "" {
				f.event["deliveryId"] = "gdl-1"
				f.event["taskUid"] = "task-uid"
				f.delivery["kind"] = tc.kind
				f.delivery["state"] = tc.deliveryState
				f.delivery["text"] = tc.text
			}
			_, c, _ := startAdapter(t, f)
			task, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("invented result: %+v", task)
				}
				return
			}
			if err != nil || task.Status.State != tc.want {
				t.Fatalf("state=%+v error=%v", task, err)
			}
		})
	}
	for _, field := range []string{
		"id", "eventId", "namespace", "namespaceUid", "gatewayUid", "gatewayName", "bindingName", "taskName",
		"sessionName", "accountId", "contextId", "threadId", "replyTarget",
	} {
		t.Run("delivery correlation/"+field, func(t *testing.T) {
			f := newLedger()
			f.complete()
			f.delivery[field] = "foreign"
			_, c, _ := startAdapter(t, f)
			if _, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID}); err == nil {
				t.Fatal("foreign output revealed")
			}
		})
	}
}

func rawRequest(t *testing.T, s *httptest.Server, path, token, version, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", s.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if version != "" {
		req.Header.Set("A2A-Version", version)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error("response body close failed")
		}
	}()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if path == "/a2a" && json.Valid(data) && resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("non-JSON RPC response: %s", resp.Header.Get("Content-Type"))
	}
	return resp.StatusCode, data
}

func TestMalformedRequestsCannotAdmit(t *testing.T) {
	f := newLedger()
	s, _, _ := startAdapter(t, f)
	body := `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":"m",` +
		`"role":"ROLE_USER","parts":[{"text":"` + string([]byte{0xff}) +
		`"}]},"configuration":{"returnImmediately":true}}}`
	for _, tc := range []struct {
		payload string
		code    int
	}{
		{body, -32700}, {`{`, -32700}, {`{} {}`, -32700}, {``, -32700}, {`null`, -32600}, {`[]`, -32602},
	} {
		status, b := rawRequest(t, s, "/a2a", "fixture-client", "1.0", tc.payload)
		assertRPCError(t, status, b, tc.code)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 {
		t.Fatal("malformed request created an event")
	}
}

func TestUnsupportedOperationsUseSDKErrorResponses(t *testing.T) {
	f := newLedger()
	s, _, _ := startAdapter(t, f)
	for _, tc := range []struct{ method, params, reason string }{
		{"ListTasks", `{}`, "UNSUPPORTED_OPERATION"},
		{"SubscribeToTask", `{"id":"gev-1"}`, "UNSUPPORTED_OPERATION"},
		{"SendStreamingMessage", `{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]}}`,
			"UNSUPPORTED_OPERATION"},
		{"GetTaskPushNotificationConfig", `{"taskId":"gev-1","id":"p"}`, "PUSH_NOTIFICATION_NOT_SUPPORTED"},
		{"ListTaskPushNotificationConfigs", `{"taskId":"gev-1"}`, "PUSH_NOTIFICATION_NOT_SUPPORTED"},
		{"CreateTaskPushNotificationConfig", `{"taskId":"gev-1","url":"https://untrusted.example"}`,
			"PUSH_NOTIFICATION_NOT_SUPPORTED"},
		{"DeleteTaskPushNotificationConfig", `{"taskId":"gev-1","id":"p"}`, "PUSH_NOTIFICATION_NOT_SUPPORTED"},
		{"GetExtendedAgentCard", `{}`, "UNSUPPORTED_OPERATION"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			_, b := rawRequest(t, s, "/a2a", "fixture-client", "1.0",
				`{"jsonrpc":"2.0","id":"1","method":"`+tc.method+`","params":`+tc.params+`}`)
			if !bytes.Contains(b, []byte(tc.reason)) {
				t.Fatalf("unsupported response: %s", b)
			}
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 {
		t.Fatal("unsupported method created event")
	}
}

func TestAuthenticationVersionAndBodyBoundaries(t *testing.T) {
	f := newLedger()
	s, _, cfg := startAdapter(t, f)
	for _, token := range []string{"", "wrong", "fixture-outbound"} {
		status, _ := rawRequest(t, s, "/a2a", token, "1.0", "not JSON")
		if status != 401 {
			t.Fatalf("auth must precede decoding: %d", status)
		}
	}
	body := `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":"m",` +
		`"role":"ROLE_USER","parts":[{"text":"Hello"}]},"configuration":{"returnImmediately":true}}}`
	for _, version := range []string{"", "0.3", "2.0"} {
		_, b := rawRequest(t, s, "/a2a", "fixture-client", version, body)
		if !bytes.Contains(b, []byte("VERSION_NOT_SUPPORTED")) {
			t.Fatalf("version silently accepted: %s", b)
		}
	}
	status, _ := rawRequest(t, s, "/a2a", "fixture-client", "1.0", body+strings.Repeat(" ", 256<<10))
	if status != 413 {
		t.Fatalf("body limit: %d", status)
	}
	if err := os.WriteFile(cfg.ClientTokenFile, []byte("fixture-rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	status, _ = rawRequest(t, s, "/a2a", "fixture-client", "1.0", body)
	if status != 401 {
		t.Fatal("stale credential accepted")
	}
	status, b := rawRequest(t, s, "/a2a", "fixture-rotated", "1.0", body)
	if status != 200 || bytes.Contains(b, []byte(`"error"`)) {
		t.Fatalf("rotated credential failed: %d %s", status, b)
	}
}
