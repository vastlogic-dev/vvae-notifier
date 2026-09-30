package notifier

// 部署串：vvaepush1.<base64url(JSON{r 中继根地址, k 推送 key, e 加密密钥})>，由 App 生成。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

const deployPrefix = "vvaepush1."

type Deploy struct {
	Relay   string
	PushKey string
	EncKey  []byte
}

var relayURLPattern = regexp.MustCompile(`^https?://[^/]+`)

func ParseDeployString(text string) (Deploy, error) {
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, deployPrefix) {
		return Deploy{}, errors.New("部署串格式不对：应以 vvaepush1. 开头")
	}
	var obj struct {
		R string `json:"r"`
		K string `json:"k"`
		E string `json:"e"`
	}
	raw, err := b64.DecodeString(s[len(deployPrefix):])
	if err != nil || json.Unmarshal(raw, &obj) != nil {
		return Deploy{}, errors.New("部署串无法解析，请从 App 里重新复制")
	}
	if !relayURLPattern.MatchString(obj.R) {
		return Deploy{}, errors.New("部署串缺少中继地址")
	}
	if k, err := b64.DecodeString(obj.K); err != nil || len(k) != 16 {
		return Deploy{}, errors.New("部署串的推送 key 不对")
	}
	if obj.E == "" {
		return Deploy{}, errors.New("部署串缺少加密密钥")
	}
	enc, err := b64.DecodeString(obj.E)
	if err != nil || len(enc) != 32 {
		return Deploy{}, errors.New("部署串的加密密钥不对")
	}
	return Deploy{Relay: strings.TrimRight(obj.R, "/"), PushKey: obj.K, EncKey: enc}, nil
}

// ParseDeployStrings 解析 VVAE_KEY：一个或多个部署串（一个 V2EX 账号一个），用逗号或空白隔开。
// 推送 key 相同的只算一个。有一个解析不了就整体报错，不带着缺账号的配置跑起来。
func ParseDeployStrings(text string) ([]Deploy, error) {
	fields := strings.FieldsFunc(text, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(fields) == 0 {
		return nil, errors.New("VVAE_KEY 里没有部署串")
	}
	var out []Deploy
	seen := map[string]bool{}
	for i, f := range fields {
		d, err := ParseDeployString(f)
		if err != nil {
			if len(fields) > 1 {
				return nil, fmt.Errorf("第 %d 个部署串：%w", i+1, err)
			}
			return nil, err
		}
		if seen[d.PushKey] {
			continue
		}
		seen[d.PushKey] = true
		out = append(out, d)
	}
	return out, nil
}

// Tag 是通道的短标识：推送 key 的 SHA-256 十六进制前 8 位，与中继日志里的通道标识一致。
func (d Deploy) Tag() string {
	sum := sha256.Sum256([]byte(d.PushKey))
	return hex.EncodeToString(sum[:])[:8]
}
