package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
)

func TestParseCmuxScrollDiagnostic(t *testing.T) {
	valid := json.RawMessage(`{"mode":"remote","duration_ms":1700,"moves":8,"delta_px":-150,"queued_lines_peak":32,"batches":3,"rpc_ms_total":700,"rpc_ms_max":400}`)
	if d, ok := parseCmuxScrollDiagnostic(valid); !ok || d.Batches != 3 {
		t.Fatalf("valid metric rejected: %+v %t", d, ok)
	}
	for _, raw := range []string{
		`{"mode":"remote","duration_ms":0}`,
		`{"mode":"remote","duration_ms":10,"queued_lines_peak":33}`,
		`{"mode":"remote","duration_ms":10,"rpc_ms_max":11}`,
		`{"mode":"other","duration_ms":10}`,
		`{"mode":"local","duration_ms":10,"moves":-1}`,
		`{"mode":"local","duration_ms":10,"delta_px":100001}`,
		`{`,
	} {
		if _, ok := parseCmuxScrollDiagnostic(json.RawMessage(raw)); ok {
			t.Fatalf("accepted invalid diagnostic: %s", raw)
		}
	}
}

func TestCmuxDiagnosticRequiresAttachmentAndRateLimits(t *testing.T) {
	handler, user, _ := testCmuxHandler()
	msg := &protocol.Message{Type: protocol.TypeCmuxDiagnostic,
		Payload: json.RawMessage(`{"mode":"local","duration_ms":10,"moves":1}`)}
	handler.handleMessage(user, msg)
	if !user.lastCmuxDiagnostic.IsZero() {
		t.Fatal("unattached user was logged")
	}
	if !handler.hub.AttachCmuxUser(user.ID, "client-1") {
		t.Fatal("attach failed")
	}
	handler.handleMessage(user, msg)
	first := user.lastCmuxDiagnostic
	if first.IsZero() {
		t.Fatal("attached user was not accepted")
	}
	handler.handleMessage(user, msg)
	if user.lastCmuxDiagnostic != first {
		t.Fatal("rate limit failed")
	}
	user.lastCmuxDiagnostic = time.Now().Add(-3 * time.Second)
	handler.handleMessage(user, msg)
	if !user.lastCmuxDiagnostic.After(first) {
		t.Fatal("rate limit did not expire")
	}
}
