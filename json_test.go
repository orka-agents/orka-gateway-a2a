// Copyright (c) 2026. MIT License - see LICENSE file for details.
package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func assertRPCError(t *testing.T, status int, body []byte, code int) {
	t.Helper()
	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if status != 200 || json.Unmarshal(body, &response) != nil || response.JSONRPC != "2.0" || response.Error == nil ||
		response.Error.Code != code || response.Error.Message == "" || response.Result != nil ||
		!bytes.Equal(response.ID, []byte("null")) {
		t.Errorf("want JSON-RPC error %d, id:null, no result; got HTTP %d %s", code, status, body)
	}
}

func wireSend(idJSON, textJSON string) string {
	return `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":` + idJSON +
		`,"role":"ROLE_USER","parts":[{"text":` + textJSON + `}]},"configuration":{"returnImmediately":true}}}`
}

func TestEscapedSurrogatesCannotCollideAtAdmission(t *testing.T) {
	f := newLedger()
	s, _, _ := startAdapter(t, f)
	for _, id := range []string{`"\ud800"`, `"\ud801"`} {
		status, b := rawRequest(t, s, "/a2a", "fixture-client", "1.0", wireSend(id, `"hello"`))
		assertRPCError(t, status, b, -32700)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 || len(f.envelopes) != 0 {
		t.Fatalf("malformed IDs normalized/admitted: %d creates, %d envelopes", f.creates, len(f.envelopes))
	}
}

func TestJSONUnicodeTextValidation(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		invalid          bool
	}{
		{"high alone", `"\ud800"`, "", true},
		{"low alone", `"\udfff"`, "", true},
		{"high plus ordinary escape", `"\ud800\u0041"`, "", true},
		{"high plus high", `"\ud800\ud801"`, "", true},
		{"reversed pair", `"\udc00\ud800"`, "", true},
		{"separated pair", `"\ud800x\udc00"`, "", true},
		{"backslash then high", `"\\\ud800"`, "", true},
		{"escaped low is not pair", `"\ud800\\udc00"`, "", true},
		{"valid pair", `"\uD83D\uDE00"`, "😀", false},
		{"pair range endpoints", `"\ud800\udc00\udbff\udfff"`, "\U00010000\U0010ffff", false},
		{"literal escape text", `"\\ud800"`, `\ud800`, false},
		{"escaped backslashes", `"\\\\ud800"`, `\\ud800`, false},
		{"quoted escape text", `"\"\\ud801\""`, `"\ud801"`, false},
		{"ordinary Unicode", `"Kia ora 世界 😀 �"`, "Kia ora 世界 😀 �", false},
		{"escaped replacement and BMP", `"\ufffd\u0061\uD7FF\uE000"`, "�a\ud7ff\ue000", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLedger()
			s, _, _ := startAdapter(t, f)
			idJSON := `"m"`
			if !tc.invalid {
				idJSON = tc.text
			} // Valid Unicode is permitted in IDs too.
			status, b := rawRequest(t, s, "/a2a", "fixture-client", "1.0", wireSend(idJSON, tc.text))
			if tc.invalid {
				assertRPCError(t, status, b, -32700)
			} else if status != 200 || bytes.Contains(b, []byte(`"error"`)) {
				t.Fatalf("valid Unicode refused: %d %s", status, b)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if tc.invalid {
				if f.creates != 0 {
					t.Fatal("malformed text admitted")
				}
			} else if f.creates != 1 || f.envelopes[0]["text"] != tc.want {
				t.Fatalf("Unicode changed at admission: %+v", f.envelopes)
			}
		})
	}
}

func TestMalformedCallbackRemainsHTTPError(t *testing.T) {
	f := newLedger()
	f.complete()
	s, _, _ := startAdapter(t, f)
	for _, body := range []string{`{`, `{} {}`, `{"text":"\ud800"}`} {
		status, b := rawRequest(t, s, "/v1/deliveries", "fixture-outbound", "", body)
		if status != 400 || bytes.Contains(b, []byte(`"jsonrpc"`)) {
			t.Fatalf("callback used A2A error contract: %d %s", status, b)
		}
	}
}
