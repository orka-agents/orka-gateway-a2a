// Copyright (c) 2026. MIT License - see LICENSE file for details.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"iter"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

type handler struct {
	cfg     config
	gateway *gatewayClient
	route   routeIdentity
}

var _ a2asrv.RequestHandler = (*handler)(nil)

func (h *handler) checkRoute(ctx context.Context) error {
	current, err := h.gateway.verifyRoute(ctx)
	if err != nil || current != h.route {
		return a2a.NewError(a2a.ErrServerError, "configured route unavailable; retry after operator review")
	}
	return nil
}
func (h *handler) SendMessage(ctx context.Context, q *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	text, err := validateSendRequest(q)
	if err != nil {
		return nil, err
	}
	m := q.Message
	if q.Config != nil {
		if q.Config.HistoryLength != nil && *q.Config.HistoryLength < 0 {
			return nil, a2a.ErrInvalidParams
		}
		if q.Config.PushConfig != nil {
			return nil, a2a.ErrPushNotificationNotSupported
		}
		if len(q.Config.AcceptedOutputModes) > 0 && !slices.Contains(q.Config.AcceptedOutputModes, textPlainMediaType) {
			return nil, a2a.ErrUnsupportedContentType
		}
	}
	ctx, cancel := context.WithTimeout(ctx, h.cfg.WaitTimeout)
	defer cancel()
	if err := h.checkRoute(ctx); err != nil {
		return nil, err
	}
	// Identity excludes text; Orka's immutable-envelope deduplication rejects
	// altered payloads instead of admitting a second task for the same messageId.
	digest := stableDigest(
		h.cfg.Namespace, h.cfg.Gateway, h.cfg.Binding, h.cfg.Agent,
		h.cfg.AccountID, h.cfg.ContextID, h.cfg.SenderID, m.ID,
	)
	thread := m.ContextID
	if thread == "" {
		thread = "a2a-" + digest
	}
	envelope := &eventEnvelope{
		ProtocolVersion: gatewayVersion, ExternalEventID: "a2a-" + digest, EventType: "text",
		AccountID: h.cfg.AccountID, ContextID: h.cfg.ContextID, ThreadID: thread,
		Text: string(text), ReplyTarget: h.cfg.ContextID,
	}
	envelope.Sender.ID = h.cfg.SenderID
	id, err := h.gateway.admit(ctx, envelope)
	if err != nil {
		if errors.Is(err, upstreamStatus(http.StatusConflict)) {
			return nil, a2a.NewError(a2a.ErrInvalidParams, "messageId already identifies a different request")
		}
		return nil, a2a.NewError(a2a.ErrServerError, "admission unavailable; retry the identical messageId and payload")
	}
	e, err := h.ownedEvent(ctx, id)
	if err != nil {
		return nil, err
	}
	// Pin the first durable admission snapshot, including while blocking. A
	// later read must never silently cross retention into another incarnation.
	taskID := publicTaskID(e)
	for {
		task, err := h.readTask(ctx, e)
		if err != nil {
			return nil, err
		}
		if (q.Config != nil && q.Config.ReturnImmediately) || task.Status.State.Terminal() {
			return task, nil
		}
		select {
		case <-ctx.Done():
			return nil, a2a.NewError(a2a.ErrServerError,
				"wait ended; work was not canceled; retry identical SendMessage or use GetTask")
		case <-time.After(h.cfg.PollInterval):
		}
		e, err = h.ownedTaskEvent(ctx, taskID)
		if err != nil {
			return nil, err
		}
	}
}
func validateSendRequest(q *a2a.SendMessageRequest) (a2a.Text, error) {
	if q == nil || q.Message == nil || q.Tenant != "" {
		return "", a2a.ErrInvalidParams
	}
	m := q.Message
	if !validIdentity(m.ID) || (m.ContextID != "" && !validIdentity(m.ContextID)) || m.Role != a2a.MessageRoleUser {
		return "", a2a.ErrInvalidParams
	}
	if m.TaskID != "" || len(m.ReferenceTasks) > 0 || len(m.Extensions) > 0 || len(m.Metadata) > 0 || len(q.Metadata) > 0 {
		return "", a2a.ErrUnsupportedOperation
	}
	if len(m.Parts) != 1 || m.Parts[0] == nil {
		return "", a2a.ErrUnsupportedContentType
	}
	part := m.Parts[0]
	text, ok := part.Content.(a2a.Text)
	if !ok || (part.MediaType != "" && part.MediaType != textPlainMediaType) ||
		part.Filename != "" || len(part.Metadata) > 0 {
		return "", a2a.ErrUnsupportedContentType
	}
	if !validText(string(text)) {
		return "", a2a.ErrInvalidParams
	}
	return text, nil
}

func stableDigest(parts ...string) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// a2a1.<raw-base64url event ID>.<raw-base64url UTC RFC3339Nano createdAt>.
// Core also uses this durable admission instant to fence its Task name. Neither
// the reusable event ID alone nor a later mutable Task UID is an admission ID.
func publicTaskID(e *gatewayEvent) a2a.TaskID {
	return a2a.TaskID("a2a1." + base64.RawURLEncoding.EncodeToString([]byte(e.ID)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(e.CreatedAt.UTC().Format(time.RFC3339Nano))))
}

func (h *handler) ownedTaskEvent(ctx context.Context, id a2a.TaskID) (*gatewayEvent, error) {
	parts := strings.Split(string(id), ".")
	if len(id) > 512 || len(parts) != 3 || parts[0] != "a2a1" {
		return nil, a2a.ErrTaskNotFound
	}
	eventID, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return nil, a2a.ErrTaskNotFound
	}
	e, err := h.ownedEvent(ctx, string(eventID))
	if err != nil {
		return nil, err
	}
	// Exact canonical comparison rejects altered timestamps and encoding aliases.
	if publicTaskID(e) != id {
		return nil, a2a.ErrTaskNotFound
	}
	return e, nil
}

func (h *handler) ownedEvent(ctx context.Context, id string) (*gatewayEvent, error) {
	if !validResourceID(id) {
		return nil, a2a.ErrTaskNotFound
	}
	var e gatewayEvent
	if err := h.gateway.read(ctx, "gateway-events", id, &e); err != nil {
		if errors.Is(err, upstreamStatus(http.StatusNotFound)) || errors.Is(err, upstreamStatus(http.StatusForbidden)) {
			return nil, a2a.ErrTaskNotFound
		}
		return nil, a2a.NewError(a2a.ErrServerError, "event unavailable")
	}
	c := h.cfg
	if e.ID != id || e.Namespace != c.Namespace || e.NamespaceUID == "" ||
		e.GatewayName != c.Gateway || e.GatewayUID != h.route.gatewayUID ||
		e.BindingName != c.Binding || e.BindingUID != h.route.bindingUID ||
		e.AgentName != c.Agent || e.AgentUID != h.route.agentUID ||
		e.AccountID != c.AccountID || e.ContextID != c.ContextID || e.SenderID != c.SenderID ||
		!validIdentity(e.ThreadID) || e.ProtocolVersion != gatewayVersion || e.EventType != "text" {
		return nil, a2a.ErrTaskNotFound
	}
	if e.CreatedAt.IsZero() {
		return nil, a2a.NewError(a2a.ErrServerError, "admission identity unavailable")
	}
	return &e, nil
}
func (h *handler) correlatedDelivery(ctx context.Context, e *gatewayEvent) (*gatewayDelivery, error) {
	unavailable := a2a.NewError(a2a.ErrServerError, "final output unavailable; retry GetTask")
	if !validResourceID(e.DeliveryID) {
		return nil, unavailable
	}
	var d gatewayDelivery
	if err := h.gateway.read(ctx, "gateway-deliveries", e.DeliveryID, &d); err != nil {
		return nil, unavailable
	}
	if d.ID != e.DeliveryID || d.IdempotencyID == "" || d.EventID != e.ID ||
		d.Namespace != e.Namespace || d.NamespaceUID != e.NamespaceUID ||
		d.GatewayUID != e.GatewayUID || d.GatewayName != e.GatewayName || d.BindingName != e.BindingName ||
		!deliveryRefsMatch(e, &d) || d.AccountID != e.AccountID || d.ContextID != e.ContextID ||
		d.ThreadID != e.ThreadID || d.ReplyTarget != e.ReplyTarget || !validText(d.Text) ||
		(d.Kind != "final" && d.Kind != deliveryKindError) {
		return nil, unavailable
	}
	switch d.State {
	case "Pending", "Sending", "Delivered", "RetryScheduled", "Failed", gatewayStateDeadLettered, gatewayStateExpired:
	default:
		return nil, unavailable
	}
	return &d, nil
}

// Core expiry omits an unverified Task; denial omits both reserved refs.
// No error path permits a conflicting nonempty ref, nor do these exceptions
// apply to completed execution results (including kind:error).
func deliveryRefsMatch(e *gatewayEvent, d *gatewayDelivery) bool {
	taskMatches := d.TaskName == e.TaskName
	sessionMatches := d.SessionName == e.SessionName
	if d.Kind == deliveryKindError {
		switch e.State {
		case gatewayStateExpired:
			taskMatches = taskMatches || d.TaskName == ""
		case gatewayStateRejected, gatewayStateDeadLettered:
			taskMatches = taskMatches || d.TaskName == ""
			sessionMatches = sessionMatches || d.SessionName == ""
		}
	}
	return taskMatches && sessionMatches
}

func (h *handler) GetTask(ctx context.Context, q *a2a.GetTaskRequest) (*a2a.Task, error) {
	if q == nil || q.ID == "" || q.Tenant != "" || (q.HistoryLength != nil && *q.HistoryLength < 0) {
		return nil, a2a.ErrInvalidParams
	}
	e, err := h.ownedTaskEvent(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	return h.readTask(ctx, e)
}

func (h *handler) readTask(ctx context.Context, e *gatewayEvent) (*a2a.Task, error) {
	var d *gatewayDelivery
	if e.State == "Completed" || ((e.State == gatewayStateRejected || e.State == gatewayStateExpired ||
		e.State == gatewayStateDeadLettered) && e.DeliveryID != "") {
		var err error
		d, err = h.correlatedDelivery(ctx, e)
		if err != nil {
			return nil, err
		}
	}
	return projectTask(e, d)
}

// Both pull responses and callback receipts use this pure projection of the
// same owned event and correlated delivery snapshot; no second read or store.
func projectTask(e *gatewayEvent, d *gatewayDelivery) (*a2a.Task, error) {
	task := &a2a.Task{ID: publicTaskID(e), ContextID: e.ThreadID}
	switch e.State {
	case "Accepted", "Queued":
		task.Status.State = a2a.TaskStateSubmitted
	case "Dispatching", "TaskCreated":
		task.Status.State = a2a.TaskStateWorking
	case gatewayStateRejected:
		task.Status.State = a2a.TaskStateRejected
	case gatewayStateExpired, gatewayStateDeadLettered:
		task.Status.State = a2a.TaskStateFailed
	case "Completed":
		if d == nil {
			return nil, a2a.NewError(a2a.ErrServerError, "final output unavailable; retry GetTask")
		}
		if d.Kind == deliveryKindError {
			task.Status.State = a2a.TaskStateFailed
			task.Status.Message = statusMessage(task, d.Text)
		} else {
			if e.TaskName == "" || e.TaskUID == "" {
				return nil, a2a.NewError(a2a.ErrServerError, "final task correlation unavailable")
			}
			// Delivery transport retries/expiry do not erase the durable execution
			// result: this adapter's external consumer pulls rather than receiving push.
			task.Status.State = a2a.TaskStateCompleted
			task.Artifacts = []*a2a.Artifact{{ID: "final", Parts: a2a.ContentParts{a2a.NewTextPart(d.Text)}}}
		}
	default:
		return nil, a2a.NewError(a2a.ErrServerError, "unrecognized event state")
	}
	if task.Status.State == a2a.TaskStateRejected ||
		(task.Status.State == a2a.TaskStateFailed && task.Status.Message == nil) {
		// Never expose StateMessage/LastError (operator diagnostics). Error text is
		// exposed only through a correlated, already sanitized Orka delivery.
		text := "Orka did not complete this request."
		if e.DeliveryID != "" {
			if d == nil || d.Kind != deliveryKindError {
				return nil, a2a.ErrServerError
			}
			text = d.Text
		}
		task.Status.Message = statusMessage(task, text)
	}
	return task, nil
}
func statusMessage(t *a2a.Task, text string) *a2a.Message {
	return &a2a.Message{
		ID: string(t.ID) + ":status", TaskID: t.ID, ContextID: t.ContextID,
		Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{a2a.NewTextPart(text)},
	}
}
func (h *handler) CancelTask(ctx context.Context, q *a2a.CancelTaskRequest) (*a2a.Task, error) {
	if q == nil || q.ID == "" || q.Tenant != "" {
		return nil, a2a.ErrInvalidParams
	}
	if _, err := h.ownedTaskEvent(ctx, q.ID); err != nil {
		return nil, err
	}
	return nil, a2a.ErrTaskNotCancelable
}

func (*handler) ListTasks(context.Context, *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return nil, a2a.ErrUnsupportedOperation
}
func (*handler) SendStreamingMessage(context.Context, *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return unsupportedStream()
}
func (*handler) SubscribeToTask(context.Context, *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return unsupportedStream()
}
func unsupportedStream() iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { yield(nil, a2a.ErrUnsupportedOperation) }
}
func (*handler) GetTaskPushConfig(context.Context, *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}
func (*handler) ListTaskPushConfigs(
	context.Context, *a2a.ListTaskPushConfigRequest,
) (*a2a.ListTaskPushConfigResponse, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}
func (*handler) CreateTaskPushConfig(context.Context, *a2a.PushConfig) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}
func (*handler) DeleteTaskPushConfig(context.Context, *a2a.DeleteTaskPushConfigRequest) error {
	return a2a.ErrPushNotificationNotSupported
}
func (*handler) GetExtendedAgentCard(context.Context, *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return nil, a2a.ErrUnsupportedOperation
}

func (h *handler) callback(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/health", "/v1/capabilities":
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/v1/health" {
			writeJSON(w, map[string]string{"status": "ok"})
			return
		}
		writeJSON(w, map[string]any{
			"protocolVersion": gatewayVersion, "adapterName": "a2a-gateway-example", "adapterVersion": "0.1.0",
			"capabilities": map[string]bool{
				"inboundText": true, "outboundText": true, "threads": true, "senderIdentity": true, "idempotentDelivery": true,
			},
		})
	case "/v1/deliveries":
		boundedJSON(http.HandlerFunc(h.receiveDelivery), false).ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}
func (h *handler) receiveDelivery(w http.ResponseWriter, r *http.Request) {
	var receipt deliveryRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&receipt) != nil || receipt.ProtocolVersion != gatewayVersion ||
		!validResourceID(receipt.DeliveryID) || !validResourceID(receipt.OriginatingEventID) {
		http.Error(w, "invalid delivery", http.StatusBadRequest)
		return
	}
	e, err := h.ownedEvent(r.Context(), receipt.OriginatingEventID)
	if errors.Is(err, a2a.ErrTaskNotFound) {
		http.Error(w, "delivery not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "ledger unavailable", http.StatusServiceUnavailable)
		return
	}
	d, err := h.correlatedDelivery(r.Context(), e)
	if err != nil {
		http.Error(w, "final output unavailable", http.StatusServiceUnavailable)
		return
	}
	if receipt.DeliveryID != d.ID || receipt.IdempotencyID != d.IdempotencyID || receipt.Kind != d.Kind ||
		receipt.AccountID != d.AccountID || receipt.ContextID != d.ContextID || receipt.ThreadID != d.ThreadID ||
		receipt.ReplyTarget != d.ReplyTarget || receipt.Text != d.Text ||
		!matchesRef(receipt.TaskRef, d.Namespace, d.TaskName) ||
		!matchesRef(receipt.SessionRef, d.Namespace, d.SessionName) || !maps.Equal(receipt.Metadata, d.Metadata) {
		http.Error(w, "delivery does not match ledger", http.StatusBadRequest)
		return
	}
	task, err := projectTask(e, d)
	if err != nil || !task.Status.State.Terminal() {
		http.Error(w, "final output unavailable", http.StatusServiceUnavailable)
		return
	}
	// Orka already committed the canonical result. Acknowledgement means available
	// for GetTask, not consumed by the external client. No second outbox or push.
	writeJSON(w, map[string]string{
		"status": "delivered", "providerMessageId": "a2a-" + stableDigest(d.GatewayUID, d.EventID, d.ID),
	})
}
func matchesRef(ref *resourceReference, namespace, name string) bool {
	if name == "" {
		return ref == nil
	}
	return ref != nil && ref.Namespace == namespace && ref.Name == name
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
