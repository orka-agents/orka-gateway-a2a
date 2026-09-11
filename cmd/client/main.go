// Copyright (c) 2026. MIT License - see LICENSE file for details.
// A deliberately small official-SDK client for this example's text-only subset.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

type options struct {
	URL, TokenFile, MessageID, Text, ContextID, TaskID string
	Immediate                                          bool
}

func main() {
	var o options
	flag.StringVar(&o.URL, "url", "", "HTTPS adapter origin (no path)")
	flag.StringVar(&o.TokenFile, "token-file", "", "external caller token file; never a token flag")
	flag.StringVar(&o.MessageID, "message-id", "", "stable message ID; reuse with identical payload on retry")
	flag.StringVar(&o.Text, "text", "Hello", "one text part")
	flag.StringVar(&o.ContextID, "context-id", "", "optional A2A context, never an Orka namespace")
	flag.StringVar(&o.TaskID, "task-id", "", "GetTask instead of SendMessage")
	flag.BoolVar(&o.Immediate, "return-immediately", false, "return after admission instead of waiting")
	caFile := flag.String("ca-file", "", "optional additional TLS CA certificate PEM file")
	flag.Parse()
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if *caFile != "" {
		pem, err := os.ReadFile(*caFile)
		if err != nil || !roots.AppendCertsFromPEM(pem) {
			fmt.Fprintln(os.Stderr, "cannot load CA file")
			os.Exit(1)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := runClient(ctx, o, &http.Client{Transport: transport}, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func runClient(ctx context.Context, o options, base *http.Client, out io.Writer) (resultErr error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("url must be an HTTPS origin")
	}
	if o.TaskID == "" && o.MessageID == "" {
		return errors.New("message-id is required for retry-safe SendMessage")
	}
	c := *base
	c.Timeout = 40 * time.Second
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c.Transport = &boundedTransport{base: rt, origin: o.URL}
	card, err := agentcard.NewResolver(&c).Resolve(ctx, o.URL)
	if err != nil {
		return errors.New("card discovery failed")
	}
	// Never let a public card redirect this configured trust-domain credential.
	if len(card.SupportedInterfaces) != 1 {
		return errors.New("expected one JSONRPC interface")
	}
	iface := card.SupportedInterfaces[0]
	if iface == nil || iface.URL != o.URL+"/a2a" || iface.ProtocolVersion != "1.0" ||
		iface.ProtocolBinding != a2a.TransportProtocolJSONRPC {
		return errors.New("card does not match the configured A2A endpoint")
	}
	c.Transport = &boundedTransport{base: rt, origin: o.URL, tokenFile: o.TokenFile}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(&c))
	if err != nil {
		return errors.New("sdk client initialization failed")
	}
	defer func() {
		// Preserve call/output failures; cleanup details may contain private data.
		if err := client.Destroy(); err != nil && resultErr == nil {
			resultErr = errors.New("sdk client cleanup failed")
		}
	}()
	var result any
	if o.TaskID != "" {
		result, err = client.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(o.TaskID)})
	} else {
		result, err = client.SendMessage(ctx, &a2a.SendMessageRequest{
			Message: &a2a.Message{
				ID: o.MessageID, ContextID: o.ContextID, Role: a2a.MessageRoleUser,
				Parts: a2a.ContentParts{a2a.NewTextPart(o.Text)},
			},
			Config: &a2a.SendMessageConfig{ReturnImmediately: o.Immediate},
		})
	}
	if err != nil {
		return errors.New("A2A call failed; work was not canceled; retry identical SendMessage or use GetTask")
	}
	return json.NewEncoder(out).Encode(result)
}

// The SDK accepts an HTTP client; bound both discovery and protocol responses,
// pin its origin, and read the caller token at request time. No TLS bypass.
type boundedTransport struct {
	base              http.RoundTripper
	origin, tokenFile string
}

func (t *boundedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme+"://"+r.URL.Host != t.origin || r.URL.User != nil {
		return nil, errors.New("unexpected credential destination")
	}
	req := r.Clone(r.Context())
	if t.tokenFile != "" {
		if r.URL.Path != "/a2a" {
			return nil, errors.New("unexpected authenticated path")
		}
		f, err := os.Open(t.tokenFile)
		if err != nil {
			return nil, errors.New("caller credential unavailable")
		}
		b, err := io.ReadAll(io.LimitReader(f, (16<<10)+1))
		_ = f.Close() // Read-only credential; the checked read has no pending writes.
		token := strings.TrimSpace(string(b))
		if err != nil || len(b) > 16<<10 || token == "" ||
			strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return nil, errors.New("invalid caller credential")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	_ = resp.Body.Close() // Read-only response; read errors are checked below.
	if err != nil || len(b) > 1<<20 {
		return nil, errors.New("response exceeds limit or is unreadable")
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, nil
}
