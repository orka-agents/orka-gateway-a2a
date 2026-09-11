// Copyright (c) 2026. MIT License - see LICENSE file for details.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func receipt() map[string]any {
	return map[string]any{
		"protocolVersion": "orka.gateway.v1", "deliveryId": "gdl-1", "idempotencyId": "gdl-1",
		"originatingEventId": "gev-1", "kind": "final", "accountId": "account", "contextId": "route",
		"threadId": "thread", "replyTarget": "route", "text": "The real gateway result.",
		"taskRef":    map[string]string{"namespace": "demo", "name": "task-1"},
		"sessionRef": map[string]string{"namespace": "demo", "name": "session-1"},
	}
}
func TestCallbacksAreAuthenticatedCorrelatedAndStateless(t *testing.T) {
	f := newLedger()
	f.complete()
	s, c, cfg := startAdapter(t, f)
	for _, path := range []string{"/v1/health", "/v1/capabilities"} {
		for _, token := range []string{"", "fixture-client", "fixture-outbound"} {
			req, _ := http.NewRequest("GET", s.URL+path, nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := s.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			want := 401
			if token == "fixture-outbound" {
				want = 200
			}
			if resp.StatusCode != want {
				t.Fatalf("probe auth %s: %d", path, resp.StatusCode)
			}
			if want == 200 && path == "/v1/capabilities" {
				var out struct {
					ProtocolVersion string `json:"protocolVersion"`
					Capabilities    struct {
						InboundText        bool `json:"inboundText"`
						OutboundText       bool `json:"outboundText"`
						IdempotentDelivery bool `json:"idempotentDelivery"`
					} `json:"capabilities"`
				}
				if json.Unmarshal(data, &out) != nil || out.ProtocolVersion != "orka.gateway.v1" ||
					!out.Capabilities.InboundText || !out.Capabilities.OutboundText || !out.Capabilities.IdempotentDelivery {
					t.Fatalf("bad gateway capabilities: %s", data)
				}
			}
		}
	}
	payload, _ := json.Marshal(receipt())
	status, _ := rawRequest(t, s, "/v1/deliveries", "fixture-client", "", string(payload))
	if status != 401 {
		t.Fatal("client token authorized callback")
	}
	status, first := rawRequest(t, s, "/v1/deliveries", "fixture-outbound", "", string(payload))
	if status != 200 {
		t.Fatalf("callback status=%d: %s", status, first)
	}
	// A new adapter instance must acknowledge identically without a receipt store.
	h, err := newAdapter(t.Context(), cfg, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	secondServer := httptest.NewTLSServer(h)
	defer secondServer.Close()
	status, second := rawRequest(t, secondServer, "/v1/deliveries", "fixture-outbound", "", string(payload))
	if status != 200 || !bytes.Equal(first, second) {
		t.Fatalf("non-idempotent receipt: %d %s %s", status, first, second)
	}
	var ack map[string]string
	if json.Unmarshal(first, &ack) != nil || ack["status"] != "delivered" || ack["providerMessageId"] == "" {
		t.Fatalf("bad delivery ack: %s", first)
	}
	got, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID})
	if err != nil || got.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("callback corrupted canonical result: %+v %v", got, err)
	}
	for _, field := range []string{
		"protocolVersion", "deliveryId", "idempotencyId", "originatingEventId", "kind",
		"accountId", "contextId", "threadId", "replyTarget", "text",
	} {
		altered := receipt()
		altered[field] = "foreign"
		b, _ := json.Marshal(altered)
		status, _ := rawRequest(t, s, "/v1/deliveries", "fixture-outbound", "", string(b))
		if status < 400 {
			t.Errorf("uncorrelated %s accepted", field)
		}
	}
	for _, field := range []string{"taskRef", "sessionRef"} {
		altered := receipt()
		altered[field] = map[string]string{"namespace": "foreign", "name": "task-1"}
		b, _ := json.Marshal(altered)
		status, _ := rawRequest(t, s, "/v1/deliveries", "fixture-outbound", "", string(b))
		if status < 400 {
			t.Errorf("foreign %s accepted", field)
		}
	}
	altered := receipt()
	altered["extraField"] = "not contract"
	b, _ := json.Marshal(altered)
	status, _ = rawRequest(t, s, "/v1/deliveries", "fixture-outbound", "", string(b))
	if status != 400 {
		t.Fatal("unknown callback field accepted")
	}
	status, _ = rawRequest(t, s, "/v1/deliveries", "fixture-outbound", "", string(payload)+strings.Repeat(" ", 256<<10))
	if status != 413 {
		t.Fatal("unbounded callback")
	}
}

func TestGatewayResponsesAreBoundedAndNeverLeakDiagnosticsOrRedirectTokens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"private diagnostic", 500, "PRIVATE ORKA DIAGNOSTIC"},
		{"wrapped event", 200, `{"event":{"id":"gev-1"}}`},
		{"missing output", 404, "PRIVATE ORKA DIAGNOSTIC"},
		{"oversize", 200, `{"text":"` + strings.Repeat("x", (1<<20)+1) + `"}`}, {"malformed", 200, `{"id":"gev-1"} trailing`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLedger()
			_, c, _ := startAdapter(t, f)
			f.mu.Lock()
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				if strings.Contains(r.URL.Path, "gateway-events") {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return true
				}
				return false
			}
			f.mu.Unlock()
			_, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID})
			if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("unsafe upstream response: %v", err)
			}
		})
	}
	t.Run("redirect", func(t *testing.T) {
		f := newLedger()
		_, c, _ := startAdapter(t, f)
		followed := false
		f.mu.Lock()
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			if strings.Contains(r.URL.Path, "gateway-events") {
				http.Redirect(w, r, "/credential-trap", http.StatusTemporaryRedirect)
				return true
			}
			if r.URL.Path == "/credential-trap" {
				followed = true
				http.Error(w, "redirect followed", 500)
				return true
			}
			return false
		}
		f.mu.Unlock()
		_, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID})
		if err == nil {
			t.Fatal("redirect accepted")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if followed {
			t.Fatal("credentials followed redirect")
		}
	})
}

func TestConfiguredRouteFailClosedAndReadinessRevalidates(t *testing.T) {
	cases := map[string]string{
		"wrong gateway": `"gatewayRef":{"name":"foreign"}`, "wrong agent": `"agentRef":{"name":"foreign"}`,
		"wrong account": `"match":{"accountId":"foreign","contextId":"route","senderId":"caller"}`,
		"wrong context": `"match":{"accountId":"account","contextId":"foreign","senderId":"caller"}`,
		"wrong sender":  `"match":{"accountId":"account","contextId":"route","senderId":"foreign"}`,
		"broad sender":  `"senderPolicy":{"mode":"all"}`,
		"wrong session": `"session":{"mode":"context"}`,
	}
	base := `{"metadata":{"namespace":"demo","name":"assistant","uid":"binding-uid"},"spec":{ ` +
		`"gatewayRef":{"name":"a2a"}, "agentRef":{"name":"assistant"}, ` +
		`"match":{"accountId":"account","contextId":"route","senderId":"caller"}, ` +
		`"senderPolicy":{"mode":"allowlist","allowedSenderIds":["caller"]}, "session":{"mode":"thread-sender"} }}`
	for name, replacement := range cases {
		t.Run(name, func(t *testing.T) {
			f := newLedger()
			s, c, cfg := startAdapter(t, f)
			r, err := c.SendMessage(t.Context(), request(true))
			admitted := taskResult(t, r, err)
			f.mu.Lock()
			f.complete()
			f.mu.Unlock()
			var obj map[string]any
			_ = json.Unmarshal([]byte(base), &obj)
			var patch map[string]any
			_ = json.Unmarshal([]byte("{"+replacement+"}"), &patch)
			maps.Copy(obj["spec"].(map[string]any), patch)
			f.mu.Lock()
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/api/v1/gatewaybindings/assistant" {
					_ = json.NewEncoder(w).Encode(obj)
					return true
				}
				return false
			}
			f.mu.Unlock()
			if _, err := newAdapter(t.Context(), cfg, &http.Client{}); err == nil {
				t.Fatal("mismatched route started")
			}
			resp, err := s.Client().Get(s.URL + "/readyz")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 503 {
				t.Fatalf("mismatched route ready: %d", resp.StatusCode)
			}
			if _, err := c.SendMessage(t.Context(), request(true)); !errors.Is(err, a2a.ErrServerError) {
				t.Fatalf("retry bypassed route drift: %v", err)
			}
			retained, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: admitted.ID})
			if err != nil || retained.Status.State != a2a.TaskStateCompleted {
				t.Fatalf("route drift hid retained result: %+v %v", retained, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.envelopes) != 1 {
				t.Fatal("retry reached admission after route drift")
			}
		})
	}
}

func TestSuppliedContextRetryAndUpstreamCredentialRotation(t *testing.T) {
	f := newLedger()
	s, c, cfg := startAdapter(t, f)
	q := request(true)
	q.Message.ContextID = "provided/thread:context"
	result, err := c.SendMessage(t.Context(), q)
	task := taskResult(t, result, err)
	if task.ContextID != "provided/thread:context" {
		t.Fatal("context not preserved")
	}
	q.Message.ContextID = "changed-context"
	_, err = c.SendMessage(t.Context(), q)
	if !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("altered context replay: %v", err)
	}
	if err := os.WriteFile(cfg.ReadTokenFile, []byte("fixture-read-rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "gateway-events") {
			if r.Header.Get("Authorization") != "Bearer fixture-read-rotated" {
				http.Error(w, "stale token", http.StatusUnauthorized)
			} else {
				_ = json.NewEncoder(w).Encode(f.event)
			}
			return true
		}
		return false
	}
	f.mu.Unlock()
	if _, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID}); err != nil {
		t.Fatalf("read credential rotation: %v", err)
	}
	if err := os.WriteFile(cfg.OutboundTokenFile, []byte("fixture-outbound-rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", s.URL+"/v1/health", nil)
	req.Header.Set("Authorization", "Bearer fixture-outbound-rotated")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("outbound token rotation failed")
	}
}

func TestHTTPRequiresJSONAndTLSConfiguration(t *testing.T) {
	f := newLedger()
	s, _, cfg := startAdapter(t, f)
	r, _ := http.NewRequest("POST", s.URL+"/a2a", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer fixture-client")
	r.Header.Set("Content-Type", "text/plain")
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 415 {
		t.Fatalf("accepted non-JSON content type: %d", resp.StatusCode)
	}
	for _, mutate := range []func(*config){
		func(c *config) { c.PublicURL = "http://insecure.example" },
		func(c *config) { c.OrkaURL = "https://user:private@example.org" },
		func(c *config) { c.ReadTokenFile = c.ClientTokenFile },
	} {
		bad := cfg
		mutate(&bad)
		if _, err := newAdapter(t.Context(), bad, &http.Client{}); err == nil {
			t.Fatal("unsafe configuration accepted")
		}
	}
	path := t.TempDir() + "/config.json"
	cfg.TLSCertFile = ""
	cfg.TLSKeyFile = ""
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := run(path); err == nil || !strings.Contains(err.Error(), "TLS") || time.Since(start) > time.Second {
		t.Fatalf("plaintext startup permitted: %v", err)
	}
}
