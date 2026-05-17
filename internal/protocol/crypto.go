package protocol

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

// TypeEncrypted 加密消息类型
const TypeEncrypted MessageType = "encrypted"

// EncryptedPayload 加密消息载荷
type EncryptedPayload struct {
	Data string `json:"data"` // base64(nonce + ciphertext + tag)
}

// DeriveKey 从字符串派生 32 字节 AES-256 密钥
// 注意：建议使用高熵随机字符串（至少 32 字符），不要使用短密码
func DeriveKey(keyStr string) []byte {
	h := sha256.Sum256([]byte(keyStr))
	return h[:]
}

// EncryptMessage 加密消息，返回加密后的 Message
// key 为 nil 时透传（不加密）
func EncryptMessage(key []byte, msg *Message) (*Message, error) {
	if key == nil || len(key) == 0 {
		return msg, nil
	}

	// 序列化原始消息
	plaintext, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}

	// AES-GCM 加密
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}

	nonce := make([]byte, aesgcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	// Seal 将 nonce 作为密文前缀，密文末尾包含 GCM 认证 tag
	ciphertext := aesgcm.Seal(nonce, nonce, plaintext, nil)

	// 封装为加密消息
	encoded := base64.StdEncoding.EncodeToString(ciphertext)
	payload, _ := json.Marshal(EncryptedPayload{Data: encoded})

	return &Message{
		Type:    TypeEncrypted,
		Payload: payload,
	}, nil
}

// DecryptMessage 解密消息，返回原始 Message
// key 为 nil 时透传（不解密）
func DecryptMessage(key []byte, msg *Message) (*Message, error) {
	if key == nil || len(key) == 0 {
		return msg, nil
	}

	if msg.Type != TypeEncrypted {
		return msg, nil
	}

	// 解析加密载荷
	var encPayload EncryptedPayload
	if err := json.Unmarshal(msg.Payload, &encPayload); err != nil {
		return nil, fmt.Errorf("unmarshal encrypted payload: %w", err)
	}

	// base64 解码
	ciphertext, err := base64.StdEncoding.DecodeString(encPayload.Data)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}

	// AES-GCM 解密
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}

	nonceSize := aesgcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	// 反序列化原始消息
	var original Message
	if err := json.Unmarshal(plaintext, &original); err != nil {
		return nil, fmt.Errorf("unmarshal original: %w", err)
	}

	return &original, nil
}
