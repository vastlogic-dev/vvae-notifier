package notifier

// V2EX API v2：GET /api/v2/notifications（通知）与 GET /api/v2/token（令牌过期时间）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const apiBase = "https://www.v2ex.com/api/v2"

type rawNotification struct {
	ID       *int64  `json:"id"`
	MemberID *int64  `json:"member_id"`
	Text     string  `json:"text"`
	Payload  *string `json:"payload"`
	Created  *int64  `json:"created"`
	Member   *struct {
		Username string `json:"username"`
	} `json:"member"`
}

var (
	// 动作 HTML 里第一个帖子链接：<a href="/t/123#reply4" ...>标题</a>
	actionTopicLink = regexp.MustCompile(`(?i)<a[^>]*href="([^"]*/t/\d+[^"]*)"[^>]*>([\s\S]*?)</a>`)
	actionMember    = regexp.MustCompile(`/member/([^"/?#]+)`)
)

func normalizeNotification(n rawNotification) (Item, bool) {
	if n.ID == nil || n.Created == nil {
		return Item{}, false
	}
	typ := classify(htmlToText(n.Text))
	link := actionTopicLink.FindStringSubmatch(n.Text)
	var href string
	var ttl *string
	if link != nil {
		href = link[1]
		if t := strings.TrimSpace(decodeEntities(htmlTagPattern.ReplaceAllString(link[2], ""))); t != "" {
			ttl = &t
		}
	}
	topic, anchor := parseTopicLink(href)
	var user string
	if n.Member != nil {
		user = n.Member.Username
	}
	if user == "" {
		if m := actionMember.FindStringSubmatch(n.Text); m != nil {
			user = m[1]
		}
	}
	var payload string
	if n.Payload != nil {
		payload = *n.Payload
	}
	return Item{
		ID:    strconv.FormatInt(*n.ID, 10),
		T:     *n.Created,
		Type:  typ,
		U:     user,
		MID:   n.MemberID,
		Topic: topic,
		Floor: floorFor(typ, anchor),
		TTL:   ttl,
		Body:  htmlToText(payload),
		Raw:   n.Text,
	}, true
}

func (p *Poller) fetchNotifications(pat, etag string) fetchResult {
	res, err := p.getV2EX(apiBase+"/notifications", etag, pat)
	if err != nil {
		return fetchFailed(netDetail(err))
	}
	defer res.Body.Close()
	remaining, resetAt := rateInfo(res.Header)
	switch {
	case res.StatusCode == http.StatusNotModified:
		return fetchResult{kind: fetchNotModified, remaining: remaining, resetAt: resetAt}
	case res.StatusCode == http.StatusUnauthorized:
		return fetchResult{kind: fetchInvalid, remaining: -1, resetAt: -1}
	case res.StatusCode == http.StatusTooManyRequests:
		return fetchResult{kind: fetchRateLimited, remaining: -1, resetAt: resetAt}
	case res.StatusCode/100 != 2:
		return fetchFailed(fmt.Sprintf("HTTP %d", res.StatusCode))
	}
	data, err := readBody(res)
	if err != nil {
		return fetchFailed(netDetail(err))
	}
	var body struct {
		Success bool              `json:"success"`
		Result  []json.RawMessage `json:"result"`
	}
	if json.Unmarshal(data, &body) != nil {
		return fetchFailed("响应不是 JSON")
	}
	if !body.Success || body.Result == nil {
		return fetchFailed("响应缺少 result")
	}
	// 逐条解析：一条格式不对不影响其他条。
	items := []Item{}
	for _, raw := range body.Result {
		var n rawNotification
		if json.Unmarshal(raw, &n) != nil {
			continue
		}
		if it, ok := normalizeNotification(n); ok {
			items = append(items, it)
		}
	}
	return fetchResult{kind: fetchOK, etag: res.Header.Get("Etag"), items: items, remaining: remaining, resetAt: resetAt}
}

type tokenInfo struct {
	expiresAt int64 // Unix 秒
	invalid   bool
	err       string
}

// fetchTokenInfo：过期时间 = created + expiration（regular 权限可查）。
func (p *Poller) fetchTokenInfo(pat string) tokenInfo {
	res, err := p.getV2EX(apiBase+"/token", "", pat)
	if err != nil {
		return tokenInfo{err: netDetail(err)}
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return tokenInfo{invalid: true}
	}
	if res.StatusCode/100 != 2 {
		return tokenInfo{err: fmt.Sprintf("HTTP %d", res.StatusCode)}
	}
	data, err := readBody(res)
	if err != nil {
		return tokenInfo{err: netDetail(err)}
	}
	var body struct {
		Result struct {
			Created    *int64 `json:"created"`
			Expiration *int64 `json:"expiration"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &body) != nil || body.Result.Created == nil || body.Result.Expiration == nil {
		return tokenInfo{err: "缺少 created/expiration"}
	}
	return tokenInfo{expiresAt: *body.Result.Created + *body.Result.Expiration}
}
