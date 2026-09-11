// Copyright (c) 2026. MIT License - see LICENSE file for details.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	gatewayVersion           = "orka.gateway.v1"
	gatewayStateRejected     = "Rejected"
	gatewayStateDeadLettered = "DeadLettered"
	gatewayStateExpired      = "Expired"
	deliveryKindError        = "error"
	maxBodyBytes             = 256 << 10
	maxTextBytes             = 64 << 10
	maxUpstreamBytes         = 1 << 20
)

// These narrow HTTP projections follow internal/gateway/protocol/types.go and
// internal/store/gateway_types.go. The independent example module cannot import
// Orka's internal packages. A2A wire types, unlike these Orka DTOs, come from the SDK.
type eventEnvelope struct {
	ProtocolVersion string `json:"protocolVersion"`
	ExternalEventID string `json:"externalEventId"`
	EventType       string `json:"eventType"`
	AccountID       string `json:"accountId"`
	ContextID       string `json:"contextId"`
	ThreadID        string `json:"threadId"`
	Sender          struct {
		ID string `json:"id"`
	} `json:"sender"`
	Text        string `json:"text"`
	ReplyTarget string `json:"replyTarget"`
}
type gatewayEvent struct {
	ID              string    `json:"id"`
	Namespace       string    `json:"namespace"`
	NamespaceUID    string    `json:"namespaceUid"`
	GatewayName     string    `json:"gatewayName"`
	GatewayUID      string    `json:"gatewayUid"`
	BindingName     string    `json:"bindingName"`
	BindingUID      string    `json:"bindingUid"`
	AgentName       string    `json:"agentName"`
	AgentUID        string    `json:"agentUid"`
	ProtocolVersion string    `json:"protocolVersion"`
	EventType       string    `json:"eventType"`
	State           string    `json:"state"`
	AccountID       string    `json:"accountId"`
	ContextID       string    `json:"contextId"`
	ThreadID        string    `json:"threadId"`
	SenderID        string    `json:"senderId"`
	ReplyTarget     string    `json:"replyTarget"`
	TaskName        string    `json:"taskName"`
	TaskUID         string    `json:"taskUid"`
	SessionName     string    `json:"sessionName"`
	DeliveryID      string    `json:"deliveryId"`
	CreatedAt       time.Time `json:"createdAt"`
}
type gatewayDelivery struct {
	ID            string            `json:"id"`
	IdempotencyID string            `json:"idempotencyId"`
	Namespace     string            `json:"namespace"`
	NamespaceUID  string            `json:"namespaceUid"`
	GatewayName   string            `json:"gatewayName"`
	GatewayUID    string            `json:"gatewayUid"`
	BindingName   string            `json:"bindingName"`
	EventID       string            `json:"eventId"`
	TaskName      string            `json:"taskName"`
	SessionName   string            `json:"sessionName"`
	Kind          string            `json:"kind"`
	State         string            `json:"state"`
	AccountID     string            `json:"accountId"`
	ContextID     string            `json:"contextId"`
	ThreadID      string            `json:"threadId"`
	ReplyTarget   string            `json:"replyTarget"`
	Text          string            `json:"text"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}
type resourceReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
type deliveryRequest struct {
	ProtocolVersion    string             `json:"protocolVersion"`
	DeliveryID         string             `json:"deliveryId"`
	IdempotencyID      string             `json:"idempotencyId"`
	OriginatingEventID string             `json:"originatingEventId"`
	TaskRef            *resourceReference `json:"taskRef,omitempty"`
	SessionRef         *resourceReference `json:"sessionRef,omitempty"`
	Kind               string             `json:"kind"`
	AccountID          string             `json:"accountId"`
	ContextID          string             `json:"contextId"`
	ThreadID           string             `json:"threadId,omitempty"`
	ReplyTarget        string             `json:"replyTarget"`
	Text               string             `json:"text"`
	Metadata           map[string]string  `json:"metadata,omitempty"`
}

type gatewayClient struct {
	cfg  config
	http *http.Client
}
type upstreamStatus int

func (s upstreamStatus) Error() string { return fmt.Sprintf("orka returned HTTP %d", int(s)) }

func newGatewayClient(cfg config, client *http.Client) *gatewayClient {
	c := *client
	c.Timeout = 10 * time.Second
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &gatewayClient{cfg: cfg, http: &c}
}

func (g *gatewayClient) call(ctx context.Context, method, path, tokenFile string, input, output any) error {
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil || len(body) > maxBodyBytes {
			return errors.New("invalid Orka request")
		}
	}
	token, err := readToken(tokenFile)
	if err != nil {
		return errors.New("orka credential unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, method, g.cfg.OrkaURL+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Orka URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return errors.New("orka request failed")
	}
	// Read-only response: status/read/decode errors determine the result, not close.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return upstreamStatus(resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBytes+1))
	if err != nil || len(data) > maxUpstreamBytes {
		return errors.New("orka response exceeds limit or is unreadable")
	}
	if json.Unmarshal(data, output) != nil {
		return errors.New("invalid Orka response")
	}
	return nil
}
func (g *gatewayClient) read(ctx context.Context, resource, id string, out any) error {
	path := "/api/v1/" + resource + "/" + url.PathEscape(id) + "?namespace=" + url.QueryEscape(g.cfg.Namespace)
	return g.call(ctx, http.MethodGet, path, g.cfg.ReadTokenFile, nil, out)
}
func (g *gatewayClient) admit(ctx context.Context, e *eventEnvelope) (string, error) {
	var ack struct {
		EventID string `json:"eventId"`
	}
	path := "/api/v1/gateways/" + g.cfg.Namespace + "/" + g.cfg.Gateway + "/events"
	err := g.call(ctx, http.MethodPost, path, g.cfg.InboundTokenFile, e, &ack)
	if err != nil {
		return "", err
	}
	if !validResourceID(ack.EventID) {
		return "", errors.New("missing Orka eventId")
	}
	return ack.EventID, nil
}

type objectMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	UID       string `json:"uid"`
}
type routeIdentity struct{ gatewayUID, bindingUID, agentUID string }

// Verify actual CR fields, not invented top-level names or serialized prompts.
// Do not wait for Gateway.status.ready: its authenticated probe calls this server.
func (g *gatewayClient) verifyRoute(ctx context.Context) (routeIdentity, error) {
	var agent, gw struct {
		Metadata objectMeta `json:"metadata"`
	}
	var binding struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			GatewayRef struct {
				Name string `json:"name"`
			} `json:"gatewayRef"`
			AgentRef struct {
				Name string `json:"name"`
			} `json:"agentRef"`
			Match struct {
				AccountID string `json:"accountId"`
				ContextID string `json:"contextId"`
				ThreadID  string `json:"threadId"`
				SenderID  string `json:"senderId"`
			} `json:"match"`
			SenderPolicy struct {
				Mode             string   `json:"mode"`
				AllowedSenderIDs []string `json:"allowedSenderIds"`
			} `json:"senderPolicy"`
			Session struct {
				Mode string `json:"mode"`
				Name string `json:"name"`
			} `json:"session"`
		} `json:"spec"`
	}
	if err := g.read(ctx, "agents", g.cfg.Agent, &agent); err != nil {
		return routeIdentity{}, err
	}
	if err := g.read(ctx, "gateways", g.cfg.Gateway, &gw); err != nil {
		return routeIdentity{}, err
	}
	if err := g.read(ctx, "gatewaybindings", g.cfg.Binding, &binding); err != nil {
		return routeIdentity{}, err
	}
	matches := func(m objectMeta, name string) bool {
		return m.Name == name && m.Namespace == g.cfg.Namespace && m.UID != ""
	}
	b := binding.Spec
	if !matches(agent.Metadata, g.cfg.Agent) || !matches(gw.Metadata, g.cfg.Gateway) ||
		!matches(binding.Metadata, g.cfg.Binding) || b.GatewayRef.Name != g.cfg.Gateway || b.AgentRef.Name != g.cfg.Agent ||
		b.Match.AccountID != g.cfg.AccountID || b.Match.ContextID != g.cfg.ContextID ||
		b.Match.SenderID != g.cfg.SenderID || b.Match.ThreadID != "" ||
		b.Session.Mode != "thread-sender" || b.Session.Name != "" ||
		(b.SenderPolicy.Mode != "allowlist" && b.SenderPolicy.Mode != "") ||
		len(b.SenderPolicy.AllowedSenderIDs) != 1 || b.SenderPolicy.AllowedSenderIDs[0] != g.cfg.SenderID {
		return routeIdentity{}, errors.New("configured Agent/Gateway/Binding route does not match")
	}
	return routeIdentity{gatewayUID: gw.Metadata.UID, bindingUID: binding.Metadata.UID, agentUID: agent.Metadata.UID}, nil
}
