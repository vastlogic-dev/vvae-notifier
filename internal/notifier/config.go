package notifier

// App 加密后经中继下发的配置：轮询源、凭据、推送类型、间隔。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"slices"
)

type Source string

const (
	SourceAPI  Source = "api"
	SourceFeed Source = "feed"
)

type Config struct {
	V        int
	Source   Source
	PAT      string
	FeedURL  string
	Types    []ItemType
	Interval int   // 秒
	WarnDays []int // 从大到小
}

var intervalLimits = map[Source]struct{ def, min int }{
	SourceAPI:  {30, 15},
	SourceFeed: {60, 60},
}

// 不允许查询参数：加参数会绕过 V2EX 的边缘缓存。
var feedURLPattern = regexp.MustCompile(`^https://(www\.)?v2ex\.com/n/[A-Za-z0-9]+\.xml$`)

// jsonInt 对应 JS 的 Number.isInteger：JSON 数字且没有小数部分。
func jsonInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, false
	}
	return int(f), true
}

func ParseConfig(data []byte) (*Config, error) {
	var o map[string]any
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, errors.New("配置不是 JSON 对象")
	}
	v, ok := jsonInt(o["v"])
	if !ok || v < 1 {
		return nil, errors.New("配置缺少版本号")
	}
	src, _ := o["source"].(string)
	source := Source(src)
	lim, ok := intervalLimits[source]
	if !ok {
		return nil, errors.New("配置的轮询源不对")
	}
	cfg := &Config{V: v, Source: source, Types: slices.Clone(AllTypes), Interval: lim.def, WarnDays: []int{7, 1}}
	if source == SourceAPI {
		pat, _ := o["pat"].(string)
		if len(pat) < 8 {
			return nil, errors.New("配置缺少 PAT")
		}
		cfg.PAT = pat
	} else {
		u, _ := o["feedURL"].(string)
		if !feedURLPattern.MatchString(u) {
			return nil, errors.New("配置的提醒源地址不对")
		}
		cfg.FeedURL = u
	}
	if arr, ok := o["types"].([]any); ok {
		cfg.Types = []ItemType{}
		for _, t := range arr {
			if s, ok := t.(string); ok && slices.Contains(AllTypes, ItemType(s)) {
				cfg.Types = append(cfg.Types, ItemType(s))
			}
		}
	}
	if n, ok := jsonInt(o["interval"]); ok {
		cfg.Interval = min(3600, max(lim.min, n))
	}
	if arr, ok := o["warnDays"].([]any); ok {
		cfg.WarnDays = []int{}
		for _, d := range arr {
			if n, ok := jsonInt(d); ok && n >= 0 && n <= 60 && !slices.Contains(cfg.WarnDays, n) {
				cfg.WarnDays = append(cfg.WarnDays, n)
			}
		}
		slices.Sort(cfg.WarnDays)
		slices.Reverse(cfg.WarnDays)
	}
	return cfg, nil
}

// sourceFingerprint 是轮询源与凭据的指纹：变了就重新记基线。
func (c *Config) sourceFingerprint() string {
	cred := c.PAT
	if c.Source == SourceFeed {
		cred = c.FeedURL
	}
	sum := sha256.Sum256([]byte(string(c.Source) + "\n" + cred))
	return hex.EncodeToString(sum[:12])
}
