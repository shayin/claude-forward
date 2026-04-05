package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

const keyDerivationPrefix = "claude-forward-e2ee-v1:"

// E2EE 端到端加密上下文
type E2EE struct {
	aead    cipher.AEAD
	enabled bool
}

// NewE2EE 创建 E2EE 实例，encryptionKey 为空则 disabled
func NewE2EE(encryptionKey string) (*E2EE, error) {
	if encryptionKey == "" {
		return &E2EE{enabled: false}, nil
	}

	// SHA-256 派生密钥
	h := sha256.New()
	h.Write([]byte(keyDerivationPrefix + encryptionKey))
	key := h.Sum(nil) // 32 bytes

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes.NewCipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cipher.NewGCM: %w", err)
	}

	return &E2EE{aead: aead, enabled: true}, nil
}

// Enabled 是否启用加密
func (e *E2EE) Enabled() bool { return e.enabled }

// EncryptPayload 加密 payload，返回 base64 编码的 nonce+ciphertext+tag
func (e *E2EE) EncryptPayload(plaintext json.RawMessage) (json.RawMessage, error) {
	if !e.enabled {
		return plaintext, nil
	}

	// nil payload 不加密
	if plaintext == nil {
		return plaintext, nil
	}

	nonce := make([]byte, e.aead.NonceSize()) // 12 bytes
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	// Seal 返回 nonce || ciphertext || tag
	sealed := e.aead.Seal(nonce, nonce, plaintext, nil)
	encoded := base64.StdEncoding.EncodeToString(sealed)
	return json.Marshal(encoded)
}

// DecryptPayload 解密 payload
func (e *E2EE) DecryptPayload(encrypted json.RawMessage) (json.RawMessage, error) {
	if !e.enabled {
		return encrypted, nil
	}

	if encrypted == nil {
		return encrypted, nil
	}

	var encoded string
	if err := json.Unmarshal(encrypted, &encoded); err != nil {
		return nil, fmt.Errorf("payload is not base64 string: %w", err)
	}

	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}

	nonceSize := e.aead.NonceSize()
	if len(sealed) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := e.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("authentication failed")
	}

	return json.RawMessage(plaintext), nil
}
