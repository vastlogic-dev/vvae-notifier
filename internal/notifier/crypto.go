package notifier

// 加密：AES-256-GCM，12 字节 nonce，AAD = "vvaepush1/<用途>"，
// 输出 base64url(nonce ‖ 密文 ‖ tag)，与 CryptoKit AES.GCM.SealedBox.combined 一致。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

// b64 是协议里所有 base64url 的编码：不带 = 填充。
var b64 = base64.RawURLEncoding

type Purpose string

const (
	PurposeConfig   Purpose = "config"
	PurposePush     Purpose = "push"
	PurposeInstance Purpose = "instance"
)

// Box 用部署串里的加密密钥加解密。
type Box struct{ aead cipher.AEAD }

func NewBox(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("加密密钥必须是 32 字节")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead}, nil
}

func aad(p Purpose) []byte { return []byte("vvaepush1/" + string(p)) }

func (b *Box) Seal(p Purpose, plaintext []byte) string {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	rand.Read(nonce) // Go 1.24 起不会返回错误
	return b64.EncodeToString(b.aead.Seal(nonce, nonce, plaintext, aad(p)))
}

func (b *Box) Open(p Purpose, blob string) ([]byte, error) {
	data, err := b64.DecodeString(blob)
	if err != nil {
		return nil, err
	}
	n := b.aead.NonceSize()
	if len(data) < n+b.aead.Overhead() {
		return nil, errors.New("密文太短")
	}
	return b.aead.Open(nil, data[:n], data[n:], aad(p))
}
