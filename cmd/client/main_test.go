// Copyright (c) 2026. MIT License - see LICENSE file for details.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

type sdkFixture struct {
	a2asrv.RequestHandler
	t *testing.T
}

func (f sdkFixture) SendMessage(_ context.Context, q *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	if q.Message.ID != "stable-message" || q.Message.Parts[0].Text() != "Hello" ||
		q.Message.ContextID != "context" || q.Config == nil || !q.Config.ReturnImmediately {
		f.t.Error("SDK request lost configured message/options")
	}
	return &a2a.Task{ID: "event-1", ContextID: "context", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}, nil
}
func (f sdkFixture) GetTask(_ context.Context, q *a2a.GetTaskRequest) (*a2a.Task, error) {
	if q.ID != "event-1" {
		f.t.Error("wrong task ID")
	}
	return &a2a.Task{
		ID: q.ID, ContextID: "context", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{{ID: "final", Parts: a2a.ContentParts{a2a.NewTextPart("answer")}}},
	}, nil
}

func TestOfficialClientDiscoverySendAndGet(t *testing.T) {
	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var origin string
	wire := a2asrv.NewJSONRPCHandler(sdkFixture{t: t})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == a2asrv.WellKnownAgentCardPath {
			if r.Header.Get("Authorization") != "" {
				t.Error("credential sent to public discovery")
			}
			a2asrv.NewStaticAgentCardHandler(&a2a.AgentCard{
				SupportedInterfaces: []*a2a.AgentInterface{
					a2a.NewAgentInterface(origin+"/a2a", a2a.TransportProtocolJSONRPC),
				},
			}).ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get(a2a.SvcParamVersion) != "1.0" {
			t.Error("missing token/version")
		}
		wire.ServeHTTP(w, r)
	}))
	defer server.Close()
	origin = server.URL
	opts := options{
		URL: origin, TokenFile: path, MessageID: "stable-message",
		Text: "Hello", ContextID: "context", Immediate: true,
	}
	var out bytes.Buffer
	if err := runClient(t.Context(), opts, server.Client(), &out); err != nil {
		t.Fatal(err)
	}
	var task a2a.Task
	if json.Unmarshal(out.Bytes(), &task) != nil || task.ID != "event-1" || task.Status.State != a2a.TaskStateSubmitted {
		t.Fatalf("bad send output: %s", out.Bytes())
	}
	opts.TaskID = "event-1"
	out.Reset()
	if err := runClient(t.Context(), opts, server.Client(), &out); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(out.Bytes(), &task) != nil || task.Artifacts[0].Parts[0].Text() != "answer" {
		t.Fatal("GetTask lost output")
	}
}

func TestClientRefusesForeignCardAndRedirect(t *testing.T) {
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"foreign card", "redirect", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			called := false
			trap := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
			defer trap.Close()
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "foreign card":
					a2asrv.NewStaticAgentCardHandler(&a2a.AgentCard{
						SupportedInterfaces: []*a2a.AgentInterface{
							a2a.NewAgentInterface(trap.URL+"/a2a", a2a.TransportProtocolJSONRPC),
						},
					}).ServeHTTP(w, r)
				case "redirect":
					http.Redirect(w, r, trap.URL, http.StatusTemporaryRedirect)
				case "oversize":
					_, _ = w.Write(bytes.Repeat([]byte("x"), (1<<20)+1))
				}
			}))
			defer s.Close()
			if err := runClient(t.Context(),
				options{URL: s.URL, TokenFile: tokenFile, MessageID: "m", Text: "x"},
				s.Client(), io.Discard); err == nil {
				t.Fatal("unsafe discovery accepted")
			}
			if called {
				t.Fatal("followed foreign endpoint")
			}
		})
	}
}
