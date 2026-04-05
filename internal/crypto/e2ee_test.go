package crypto

import (
	"encoding/json"
	"testing"
)

func TestE2EE_Disabled(t *testing.T) {
	e, err := NewE2EE("")
	if err != nil {
		t.Fatal(err)
	}
	if e.Enabled() {
		t.Error("should be disabled with empty key")
	}

	// 加密/解密应透传
	original := json.RawMessage(`{"text":"hello"}`)
	encrypted, err := e.EncryptPayload(original)
	if err != nil {
		t.Fatal(err)
	}
	if string(encrypted) != string(original) {
		t.Error("disabled E2EE should pass through")
	}
}

func TestE2EE_EncryptDecrypt(t *testing.T) {
	e, err := NewE2EE("test-secret-key")
	if err != nil {
		t.Fatal(err)
	}
	if !e.Enabled() {
		t.Error("should be enabled")
	}

	original := json.RawMessage(`{"text":"hello world","count":42}`)
	encrypted, err := e.EncryptPayload(original)
	if err != nil {
		t.Fatal(err)
	}

	// 加密后应该是 base64 字符串
	var str string
	if err := json.Unmarshal(encrypted, &str); err != nil {
		t.Fatalf("encrypted payload should be a string: %v", err)
	}
	if str == string(original) {
		t.Error("encrypted should differ from original")
	}

	// 解密应还原
	decrypted, err := e.DecryptPayload(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if string(decrypted) != string(original) {
		t.Errorf("decrypted mismatch: got %s, want %s", decrypted, original)
	}
}

func TestE2EE_NilPayload(t *testing.T) {
	e, err := NewE2EE("test-key")
	if err != nil {
		t.Fatal(err)
	}

	encrypted, err := e.EncryptPayload(nil)
	if err != nil {
		t.Fatal(err)
	}
	if encrypted != nil {
		t.Error("nil payload should stay nil")
	}

	decrypted, err := e.DecryptPayload(nil)
	if err != nil {
		t.Fatal(err)
	}
	if decrypted != nil {
		t.Error("nil payload should stay nil")
	}
}

func TestE2EE_WrongKey(t *testing.T) {
	e1, _ := NewE2EE("correct-key")
	e2, _ := NewE2EE("wrong-key")

	plaintext := json.RawMessage(`{"secret":"data"}`)
	encrypted, _ := e1.EncryptPayload(plaintext)

	_, err := e2.DecryptPayload(encrypted)
	if err == nil {
		t.Error("decrypt with wrong key should fail")
	}
}
