// Copyright (c) 2026. MIT License - see LICENSE file for details.
// This standalone example uses Orka's ledger, not an A2A execution manager.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

const (
	textPlainMediaType = "text/plain"
	bearerAuthScheme   = "bearer"
)

type cardDeclaration struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Version     string           `json:"version"`
	Skills      []a2a.AgentSkill `json:"skills"`
}
type config struct {
	ListenAddr        string          `json:"listenAddr"`
	TLSCertFile       string          `json:"tlsCertFile"`
	TLSKeyFile        string          `json:"tlsKeyFile"`
	PublicURL         string          `json:"publicURL"`
	OrkaURL           string          `json:"orkaURL"`
	Namespace         string          `json:"namespace"`
	Gateway           string          `json:"gateway"`
	Binding           string          `json:"binding"`
	Agent             string          `json:"agent"`
	AccountID         string          `json:"accountId"`
	ContextID         string          `json:"contextId"`
	SenderID          string          `json:"senderId"`
	ClientTokenFile   string          `json:"clientTokenFile"`
	InboundTokenFile  string          `json:"inboundTokenFile"`
	OutboundTokenFile string          `json:"outboundTokenFile"`
	ReadTokenFile     string          `json:"readTokenFile"`
	Card              cardDeclaration `json:"card"`
	PollInterval      time.Duration   `json:"-"`
	WaitTimeout       time.Duration   `json:"-"`
}

func main() {
	path := flag.String("config", "config.json", "non-secret configuration file")
	flag.Parse()
	if err := run(*path); err != nil {
		slog.Error("adapter stopped", "error", err)
		os.Exit(1)
	}
}
func run(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("cannot open configuration")
	}
	// Read-only file: read errors are checked below; close has no writes to flush.
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBodyBytes+1))
	if err != nil || len(data) > maxBodyBytes {
		return errors.New("cannot read bounded configuration")
	}
	var cfg config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if !json.Valid(data) || dec.Decode(&cfg) != nil {
		return errors.New("invalid configuration JSON")
	}
	cfg.PollInterval = time.Second
	cfg.WaitTimeout = 30 * time.Second
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8443"
	}
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		return errors.New("TLS certificate and key files are required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return errors.New("cannot load TLS certificate/key")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	h, err := newAdapter(ctx, cfg, &http.Client{})
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: cfg.ListenAddr, Handler: h, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 45 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("A2A gateway example listening with TLS")
	err = server.ListenAndServeTLS("", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return errors.New("TLS listener failed")
}

func (c config) validate() error {
	for _, v := range []string{c.Namespace, c.Gateway, c.Binding, c.Agent} {
		if !validResourceID(v) {
			return errors.New("invalid configured resource identity")
		}
	}
	for _, v := range []string{c.AccountID, c.ContextID, c.SenderID} {
		if !validIdentity(v) {
			return errors.New("invalid configured gateway identity")
		}
	}
	for _, v := range []string{c.PublicURL, c.OrkaURL} {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" ||
			(u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("configured URLs must be origins without credentials, path, query or fragment")
		}
	}
	if !strings.HasPrefix(c.PublicURL, "https://") {
		return errors.New("public URL must use HTTPS")
	}
	if c.PollInterval <= 0 || c.WaitTimeout <= 0 || c.WaitTimeout > 2*time.Minute {
		return errors.New("invalid polling bounds")
	}
	if err := c.Card.validate(); err != nil {
		return err
	}
	seen := map[[32]byte]bool{}
	for _, path := range []string{c.ClientTokenFile, c.InboundTokenFile, c.OutboundTokenFile, c.ReadTokenFile} {
		token, err := readToken(path)
		if err != nil {
			return errors.New("four distinct readable token files are required")
		}
		hash := sha256.Sum256([]byte(token))
		if seen[hash] {
			return errors.New("credential roles must use distinct tokens")
		}
		seen[hash] = true
	}
	return nil
}

func (c cardDeclaration) validate() error {
	if c.Name == "" || c.Description == "" || c.Version == "" || len(c.Skills) == 0 {
		return errors.New("reviewed card name, description, version and skills are required")
	}
	for _, skill := range c.Skills {
		if skill.ID == "" || skill.Name == "" || skill.Description == "" || len(skill.Tags) == 0 ||
			len(skill.InputModes) > 0 || len(skill.OutputModes) > 0 || len(skill.SecurityRequirements) > 0 {
			return errors.New("skills require reviewed identity, description and tags; modes/security come from the adapter")
		}
	}
	return nil
}

var resourceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,252}$`)

func validResourceID(s string) bool { return resourceIDPattern.MatchString(s) }
func validIdentity(s string) bool {
	return s != "" && len(s) <= 256 && strings.TrimSpace(s) == s && safeText(s, false)
}
func safeText(s string, multiline bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && (!multiline || (r != '\n' && r != '\r' && r != '\t')) {
			return false
		}
	}
	return true
}
func validText(s string) bool { return s != "" && len(s) <= maxTextBytes && safeText(s, true) }

// Reopen projected files on each request so Secret/ServiceAccount rotation works.
func readToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("credential unavailable")
	}
	// Read-only credential: close cannot flush data; never expose file error details.
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, (16<<10)+1))
	if err != nil || len(b) > 16<<10 {
		return "", errors.New("invalid credential file")
	}
	s := strings.TrimSpace(string(b))
	if s == "" || strings.IndexFunc(s, unicode.IsSpace) >= 0 || !safeText(s, false) {
		return "", errors.New("invalid credential file")
	}
	return s, nil
}
func authenticate(path string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := readToken(path)
		expected := sha256.Sum256([]byte("Bearer " + token))
		provided := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if err != nil || len(r.Header.Values("Authorization")) != 1 ||
			subtle.ConstantTimeCompare(expected[:], provided[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="a2a-gateway"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func boundedJSON(next http.Handler, jsonRPC bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
			return
		}
		if r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity" {
			http.Error(w, "content encoding unsupported", http.StatusUnsupportedMediaType)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		_ = r.Body.Close() // Read-only body; the read error controls request acceptance.
		if len(b) > maxBodyBytes {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		// encoding/json otherwise replaces malformed UTF-8 before text validation.
		if err != nil || !utf8.Valid(b) || !json.Valid(b) || !pairedUnicodeEscapes(b) {
			if jsonRPC {
				// The SDK decodes only one document and normalizes malformed Unicode.
				// Reject here without reflecting any untrusted request ID or content.
				writeJSON(w, map[string]any{
					"jsonrpc": "2.0", "id": nil,
					"error": map[string]any{"code": -32700, "message": "Parse error"},
				})
			} else {
				http.Error(w, "invalid JSON body", http.StatusBadRequest)
			}
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		next.ServeHTTP(w, r)
	})
}

// pairedUnicodeEscapes runs only after json.Valid. Syntax, escape lengths and
// hex digits are already checked; this scan adds just the UTF-16 pairing rule
// that encoding/json otherwise loses by replacing lone surrogates with U+FFFD.
func pairedUnicodeEscapes(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		i++
		if b[i] != 'u' {
			continue // Includes escaped backslashes: literal backslash-u is text.
		}
		unit, _ := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
		i += 4
		switch {
		case unit >= 0xDC00 && unit <= 0xDFFF:
			return false // Low surrogate without a preceding high surrogate.
		case unit >= 0xD800 && unit <= 0xDBFF:
			if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
				return false
			}
			low, _ := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
			if low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 6
		}
	}
	return true
}

// SDK middleware enforces service parameters for every known method, including
// unsupported operations. No default handler/taskstore/executor is constructed.
type protocolGate struct {
	a2asrv.PassthroughCallInterceptor
}

func (protocolGate) Before(
	ctx context.Context, cc *a2asrv.CallContext, _ *a2asrv.Request,
) (context.Context, any, error) {
	versions, _ := cc.ServiceParams().Get(a2a.SvcParamVersion)
	if len(versions) != 1 || versions[0] != string(a2a.Version) {
		return ctx, nil, a2a.ErrVersionNotSupported
	}
	if extensions, ok := cc.ServiceParams().Get(a2a.SvcParamExtensions); ok && len(extensions) > 0 {
		return ctx, nil, a2a.ErrUnsupportedOperation
	}
	if cc.Tenant() != "" {
		return ctx, nil, a2a.ErrInvalidParams
	}
	return ctx, nil, nil
}

func newAdapter(ctx context.Context, cfg config, client *http.Client) (http.Handler, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	g := newGatewayClient(cfg, client)
	route, err := g.verifyRoute(ctx)
	if err != nil {
		return nil, err
	}
	h := &handler{cfg: cfg, gateway: g, route: route}
	card := &a2a.AgentCard{
		Name: cfg.Card.Name, Description: cfg.Card.Description, Version: cfg.Card.Version, Skills: cfg.Card.Skills,
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(cfg.PublicURL+"/a2a", a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{textPlainMediaType}, DefaultOutputModes: []string{textPlainMediaType},
		SecuritySchemes: a2a.NamedSecuritySchemes{
			bearerAuthScheme: a2a.HTTPAuthSecurityScheme{Scheme: bearerAuthScheme},
		},
		SecurityRequirements: a2a.SecurityRequirementsOptions{{bearerAuthScheme: {}}},
	}
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	wire := a2asrv.NewJSONRPCHandler(&a2asrv.InterceptedHandler{
		Handler: h, Interceptors: []a2asrv.CallInterceptor{protocolGate{}},
	})
	mux.Handle("/a2a", authenticate(cfg.ClientTokenFile, boundedJSON(wire, true)))
	mux.Handle("/v1/", authenticate(cfg.OutboundTokenFile, http.HandlerFunc(h.callback)))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := h.checkRoute(r.Context()); err != nil {
			http.Error(w, "route unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux, nil
}
