package protocol

import (
	"encoding/json"
	"testing"
)

func TestDeriveKey(t *testing.T) {
	key := DeriveKey("test-password")
	if len(key) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(key))
	}

	// 相同输入产生相同密钥
	key2 := DeriveKey("test-password")
	if string(key) != string(key2) {
		t.Fatal("same input should produce same key")
	}

	// 不同输入产生不同密钥
	key3 := DeriveKey("other-password")
	if string(key) == string(key3) {
		t.Fatal("different input should produce different key")
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := DeriveKey("my-secret-key")

	original := &Message{
		Type: TypeChatInput,
		Payload: func() json.RawMessage {
			p, _ := json.Marshal(map[string]string{"text": "你好世界"})
			return p
		}(),
	}

	encrypted, err := EncryptMessage(key, original)
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}

	// 加密后类型应该是 encrypted
	if encrypted.Type != TypeEncrypted {
		t.Fatalf("expected type %s, got %s", TypeEncrypted, encrypted.Type)
	}

	// 密文载荷应该包含 base64 数据
	var encPayload EncryptedPayload
	if err := json.Unmarshal(encrypted.Payload, &encPayload); err != nil {
		t.Fatalf("unmarshal encrypted payload: %v", err)
	}
	if encPayload.Data == "" {
		t.Fatal("encrypted data should not be empty")
	}

	// 解密
	decrypted, err := DecryptMessage(key, encrypted)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}

	if decrypted.Type != original.Type {
		t.Fatalf("expected type %s, got %s", original.Type, decrypted.Type)
	}

	// 比较 payload
	var origPayload map[string]string
	var decPayload map[string]string
	json.Unmarshal(original.Payload, &origPayload)
	json.Unmarshal(decrypted.Payload, &decPayload)

	if origPayload["text"] != decPayload["text"] {
		t.Fatalf("payload mismatch: expected %v, got %v", origPayload, decPayload)
	}
}

func TestEncryptNilKeyPassthrough(t *testing.T) {
	msg := &Message{
		Type:    TypeChatInput,
		Payload: json.RawMessage(`{"text":"hello"}`),
	}

	// nil key 应透传
	result, err := EncryptMessage(nil, msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != msg {
		t.Fatal("nil key should pass through")
	}

	result, err = DecryptMessage(nil, msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != msg {
		t.Fatal("nil key should pass through on decrypt")
	}
}

func TestEncryptEmptyKeyPassthrough(t *testing.T) {
	msg := &Message{
		Type:    TypeChatInput,
		Payload: json.RawMessage(`{"text":"hello"}`),
	}

	result, err := EncryptMessage([]byte{}, msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != msg {
		t.Fatal("empty key should pass through")
	}
}

func TestDecryptWithWrongKey(t *testing.T) {
	key1 := DeriveKey("correct-key")
	key2 := DeriveKey("wrong-key")

	msg := &Message{
		Type:    TypeChatInput,
		Payload: json.RawMessage(`{"text":"secret"}`),
	}

	encrypted, err := EncryptMessage(key1, msg)
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}

	// 用错误密钥解密应该失败
	_, err = DecryptMessage(key2, encrypted)
	if err == nil {
		t.Fatal("decrypt with wrong key should fail")
	}
}

func TestEncryptProducesDifferentCiphertext(t *testing.T) {
	key := DeriveKey("test-key")

	msg := &Message{
		Type:    TypeChatInput,
		Payload: json.RawMessage(`{"text":"same content"}`),
	}

	enc1, _ := EncryptMessage(key, msg)
	enc2, _ := EncryptMessage(key, msg)

	// 相同明文，不同 nonce，密文应该不同
	var p1, p2 EncryptedPayload
	json.Unmarshal(enc1.Payload, &p1)
	json.Unmarshal(enc2.Payload, &p2)

	if p1.Data == p2.Data {
		t.Fatal("same plaintext should produce different ciphertext due to random nonce")
	}
}

func TestDecryptNonEncryptedPassthrough(t *testing.T) {
	key := DeriveKey("test-key")

	msg := &Message{
		Type:    TypeChatInput,
		Payload: json.RawMessage(`{"text":"hello"}`),
	}

	// 非 encrypted 类型的消息在解密时应该透传
	result, err := DecryptMessage(key, msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Type != TypeChatInput {
		t.Fatal("non-encrypted message should pass through decrypt")
	}
}
