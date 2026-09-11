// Copyright (c) 2026. MIT License - see LICENSE file for details.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestTwoMessageIDsShareContextWithoutAliasing(t *testing.T) {
	f := newLedger()
	f.complete()
	// Only this cardinality test needs a multi-event HTTP boundary. Key immutable
	// envelopes by externalEventId, not context; give each event its own result.
	type admission struct{ envelope, event, delivery map[string]any }
	admissions := map[string]admission{}
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v1/gateways/demo/a2a/events" {
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture-inbound" {
				http.Error(w, "bad admission", http.StatusUnauthorized)
				return true
			}
			var env map[string]any
			if json.NewDecoder(r.Body).Decode(&env) != nil {
				http.Error(w, "bad envelope", 400)
				return true
			}
			f.envelopes = append(f.envelopes, env)
			external, _ := env["externalEventId"].(string)
			a, exists := admissions[external]
			if exists && !reflect.DeepEqual(a.envelope, env) {
				http.Error(w, "immutable conflict", http.StatusConflict)
				return true
			}
			if !exists {
				f.creates++
				a = admission{env, maps.Clone(f.event), maps.Clone(f.delivery)}
				a.event["id"], a.event["deliveryId"] = fmt.Sprintf("gev-%d", f.creates), fmt.Sprintf("gdl-%d", f.creates)
				a.event["taskName"], a.event["taskUid"] =
					fmt.Sprintf("task-%d", f.creates), fmt.Sprintf("task-uid-%d", f.creates)
				a.event["threadId"], a.event["externalEventId"] = env["threadId"], external
				a.delivery["id"], a.delivery["idempotencyId"], a.delivery["eventId"] =
					a.event["deliveryId"], a.event["deliveryId"], a.event["id"]
				a.delivery["taskName"], a.delivery["threadId"], a.delivery["text"] =
					a.event["taskName"], env["threadId"], fmt.Sprintf("answer %d", f.creates)
				admissions[external] = a
			}
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"eventId": a.event["id"], "status": "accepted", "state": a.event["state"],
			})
			return true
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/gateway-events/") ||
			strings.HasPrefix(r.URL.Path, "/api/v1/gateway-deliveries/") {
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-read" ||
				r.URL.Query().Get("namespace") != "demo" {
				http.Error(w, "bad read", http.StatusUnauthorized)
				return true
			}
			for _, a := range admissions {
				for _, resource := range []struct {
					path string
					row  map[string]any
				}{{"gateway-events", a.event}, {"gateway-deliveries", a.delivery}} {
					if r.URL.Path == "/api/v1/"+resource.path+"/"+resource.row["id"].(string) {
						_ = json.NewEncoder(w).Encode(resource.row)
						return true
					}
				}
			}
			http.NotFound(w, r)
			return true
		}
		return false
	}
	_, c, _ := startAdapter(t, f)
	firstRequest := request(true)
	firstRequest.Message.ContextID = "shared-context"
	secondRequest := request(true)
	secondRequest.Message.ID = "message-2"
	secondRequest.Message.ContextID = "shared-context"
	secondRequest.Message.Parts[0] = a2a.NewTextPart("Continue")
	r, err := c.SendMessage(t.Context(), firstRequest)
	first := taskResult(t, r, err)
	r, err = c.SendMessage(t.Context(), secondRequest)
	second := taskResult(t, r, err)
	if first.ID == second.ID || first.ContextID != "shared-context" || second.ContextID != first.ContextID {
		t.Fatalf("context collapsed task cardinality: %+v %+v", first, second)
	}
	_, restarted, _ := startAdapter(t, f)
	for i, q := range []*a2a.SendMessageRequest{firstRequest, secondRequest} {
		want := []*a2a.Task{first, second}[i]
		r, err := restarted.SendMessage(t.Context(), q)
		retry := taskResult(t, r, err)
		got, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: want.ID})
		if err != nil || retry.ID != want.ID || got.ID != want.ID || got.ContextID != "shared-context" ||
			len(got.Artifacts) != 1 || got.Artifacts[0].Parts[0].Text() != []string{"answer 1", "answer 2"}[i] {
			t.Fatalf("independent task result/retry lost: %+v %v", got, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 2 || len(f.envelopes) != 4 {
		t.Fatalf("two messages must admit exactly twice, including restart/retries: creates=%d envelopes=%d",
			f.creates, len(f.envelopes))
	}
	if f.envelopes[0]["externalEventId"] == f.envelopes[1]["externalEventId"] ||
		f.envelopes[0]["threadId"] != f.envelopes[1]["threadId"] {
		t.Fatal("wrong external identity or context boundary")
	}
}

func TestPublicTaskIDFencesAdmissionIncarnation(t *testing.T) {
	f := newLedger()
	_, c, _ := startAdapter(t, f)
	r, err := c.SendMessage(t.Context(), request(true))
	first := taskResult(t, r, err)
	f.mu.Lock()
	f.complete()
	f.mu.Unlock()
	terminal, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: first.ID})
	if err != nil || terminal.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("old execution: %+v %v", terminal, err)
	}
	_, restarted, _ := startAdapter(t, f)
	r, err = restarted.SendMessage(t.Context(), request(true))
	retry := taskResult(t, r, err)
	if retry.ID != first.ID {
		t.Fatal("restart changed retained admission identity")
	}
	// The full row and tombstone have expired. Core reuses eventID, but reserves
	// another Task name from the new admission instant (service.go gatewayTaskName).
	f.mu.Lock()
	f.envelopes = nil
	f.creates = 0
	f.event["createdAt"] = "2026-02-02T03:04:05.123456789Z"
	f.event["state"], f.event["taskName"] = "Queued", "task-2"
	delete(f.event, "taskUid")
	delete(f.event, "deliveryId")
	f.mu.Unlock()
	r, err = restarted.SendMessage(t.Context(), request(true))
	second := taskResult(t, r, err)
	if second.ID == first.ID {
		t.Error("reused event ID aliases a new execution")
	}
	if _, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: first.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("old terminal ID resolves new execution: %v", err)
	}
	_, err = restarted.CancelTask(t.Context(), &a2a.CancelTaskRequest{ID: first.ID})
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("cancel ignored admission fence: %v", err)
	}
	got, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: second.ID})
	if err != nil || got.Status.State != a2a.TaskStateSubmitted {
		t.Fatalf("new execution unavailable: %+v %v", got, err)
	}
}

func TestPublicTaskIDRequiresExactFence(t *testing.T) {
	f := newLedger()
	f.complete()
	_, c, _ := startAdapter(t, f)
	for _, id := range []string{
		"gev-1", "a2a1.Z2V2LTE", fixtureTaskID + "=", fixtureTaskID + ".extra",
		strings.Replace(fixtureTaskID, "a2a1.", "a2a2.", 1),
		strings.Replace(fixtureTaskID, "Z2V2LTE", "Z2V2LTE=", 1),
		strings.Replace(fixtureTaskID, "Z2V2LTE", "Z2V2LTF", 1),     // Noncanonical unused base64 bits.
		strings.Replace(fixtureTaskID, "Z2V2LTE", "Li4vZ2V2LTE", 1), // ../gev-1.
		"a2a1.Z2V2LTE.MjAyNi0wMS0wMlQwMzowNDowNVoa",                 // Not the precise durable timestamp.
	} {
		if _, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: a2a.TaskID(id)}); !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Errorf("invalid/aliased fence resolved: %q %v", id, err)
		}
	}
	f.mu.Lock()
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v1/gateway-events/gev-1" {
			http.NotFound(w, r)
			return true
		}
		return false
	}
	f.mu.Unlock()
	if _, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("full-row retention loss hidden: %v", err)
	}
}

func TestAdmissionRequiresDurableCreatedAt(t *testing.T) {
	for _, createdAt := range []any{nil, "0001-01-01T00:00:00Z", "invalid"} {
		f := newLedger()
		f.complete()
		f.event["createdAt"] = createdAt
		s, c, _ := startAdapter(t, f)
		q := request(true)
		q.Message.ContextID = "thread"
		if _, err := c.SendMessage(t.Context(), q); !errors.Is(err, a2a.ErrServerError) {
			t.Errorf("unfenced admission exposed: %v", err)
		}
		b, _ := json.Marshal(receipt())
		if status, _ := rawRequest(t, s, "/v1/deliveries", "fixture-outbound", "", string(b)); status != 503 {
			t.Errorf("unfenced result acknowledged: %d", status)
		}
	}
}

func TestCoreErrorDeliveryReferences(t *testing.T) {
	for _, tc := range []struct {
		name, state, taskName, sessionName string
		clearEventTask, taskUID, wantError bool
	}{
		{"unverified expiry", "Expired", "", "session-1", false, false, false},
		{"deleted linked task expiry", "Expired", "", "session-1", false, true, false},
		{"verified expiry", "Expired", "task-1", "session-1", false, true, false},
		{"queue overflow", "DeadLettered", "", "", true, false, false},
		{"denial with reserved names", "DeadLettered", "", "", false, false, false},
		{"owned rejection", "Rejected", "", "", false, false, false},
		{"expiry conflicting task", "Expired", "foreign", "session-1", false, true, true},
		{"expiry conflicting session", "Expired", "", "foreign", false, false, true},
		{"expiry missing session", "Expired", "", "", false, false, true},
		{"denial conflicting task", "DeadLettered", "foreign", "", false, false, true},
		{"denial conflicting session", "DeadLettered", "", "foreign", true, false, true},
		{"completed error missing task", "Completed", "", "session-1", false, true, true},
		{"completed error missing session", "Completed", "task-1", "", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLedger()
			f.event["state"], f.event["deliveryId"] = tc.state, "gdl-1"
			if tc.clearEventTask {
				delete(f.event, "taskName")
			}
			if tc.taskUID {
				f.event["taskUid"] = "task-uid"
			}
			f.delivery["kind"], f.delivery["text"] = "error", "Safe failure text."
			for field, name := range map[string]string{"taskName": tc.taskName, "sessionName": tc.sessionName} {
				delete(f.delivery, field)
				if name != "" {
					f.delivery[field] = name
				}
			}
			metadata := map[string]string{"eventId": "gev-1"}
			if tc.state == "Expired" && tc.taskName == "task-1" {
				metadata["taskName"] = "task-1"
			}
			f.delivery["metadata"] = metadata
			r := receipt()
			r["kind"], r["text"], r["metadata"] = "error", "Safe failure text.", metadata
			for field, name := range map[string]string{"taskRef": tc.taskName, "sessionRef": tc.sessionName} {
				delete(r, field)
				if name != "" {
					r[field] = map[string]string{"namespace": "demo", "name": name}
				}
			}
			s, c, _ := startAdapter(t, f)
			task, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID})
			b, _ := json.Marshal(r)
			status, body := rawRequest(t, s, "/v1/deliveries", "fixture-outbound", "", string(b))
			if tc.wantError {
				if err == nil || status != 503 {
					t.Fatalf("conflicting/unjustified refs exposed: task=%+v err=%v callback=%d", task, err, status)
				}
				return
			}
			want := a2a.TaskStateFailed
			if tc.state == "Rejected" {
				want = a2a.TaskStateRejected
			}
			if err != nil || task.Status.State != want || task.Status.Message == nil ||
				task.Status.Message.Parts[0].Text() != "Safe failure text." {
				t.Errorf("legitimate core error unavailable: %+v %v", task, err)
			}
			if status != 200 {
				t.Errorf("legitimate core callback refused: %d %s", status, body)
			}
			// Receipt refs still have to match the omissions in the durable delivery.
			r["taskRef"] = map[string]string{"namespace": "demo", "name": "foreign"}
			b, _ = json.Marshal(r)
			status, _ = rawRequest(t, s, "/v1/deliveries", "fixture-outbound", "", string(b))
			if status != 400 {
				t.Fatalf("conflicting receipt accepted: %d", status)
			}
		})
	}
}

func TestCallbackRequiresProjectableTerminalResult(t *testing.T) {
	for _, tc := range []struct {
		name, state, kind, transport string
		removeUID, wantError         bool
	}{
		{"missing Task UID", "Completed", "final", "Pending", true, true},
		{"unknown event state", "Unknown", "final", "Pending", false, true},
		{"nonterminal event", "Queued", "final", "Pending", false, true},
		{"error without UID", "Completed", "error", "Pending", true, false},
		{"failed transport retains final", "Completed", "final", "Failed", false, false},
		{"deadletter transport retains final", "Completed", "final", "DeadLettered", false, false},
		{"expired transport retains final", "Completed", "final", "Expired", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLedger()
			f.complete()
			f.event["state"], f.delivery["kind"], f.delivery["state"] = tc.state, tc.kind, tc.transport
			if tc.removeUID {
				delete(f.event, "taskUid")
			}
			s, c, _ := startAdapter(t, f)
			r := receipt()
			r["kind"] = tc.kind
			b, _ := json.Marshal(r)
			status, body := rawRequest(t, s, "/v1/deliveries", "fixture-outbound", "", string(b))
			task, err := c.GetTask(t.Context(), &a2a.GetTaskRequest{ID: fixtureTaskID})
			if tc.wantError {
				if status != 503 {
					t.Errorf("unavailable result acknowledged: %d %s", status, body)
				}
				if tc.state != "Queued" && err == nil {
					t.Fatal("unprojectable GetTask succeeded")
				}
			} else {
				want := a2a.TaskStateCompleted
				if tc.kind == "error" {
					want = a2a.TaskStateFailed
				}
				if status != 200 || err != nil || task.Status.State != want {
					t.Fatalf("durable result lost: %d %+v %v", status, task, err)
				}
			}
			f.mu.Lock()
			reads := f.reads
			f.mu.Unlock()
			if reads != 2 {
				t.Fatalf("callback/GetTask added event reads: %d", reads)
			}
		})
	}
}
