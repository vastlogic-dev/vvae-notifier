package notifier

// V2EX 提醒 Atom Feed：https://www.v2ex.com/n/<令牌>.xml
// 注意：不给地址加任何查询参数（会绕过 V2EX 的 150 秒边缘缓存）。
// 用正则逐条取字段而不是 XML 解析器：一条内容里有非法 XML 字符时，不拖垮整个 Feed。

import (
	"bytes"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var (
	feedEntry = regexp.MustCompile(`<entry>([\s\S]*?)</entry>`)
	feedLink  = regexp.MustCompile(`<link[^>]*href="([^"]*)"`)
	feedCDATA = regexp.MustCompile(`^\s*<!\[CDATA\[([\s\S]*?)\]\]>\s*$`)
	feedElems = map[string]*regexp.Regexp{}

	// 从标题里取帖子标题。
	feedTitles = []*regexp.Regexp{
		regexp.MustCompile(`^\S+ 在回复 (.+) 时提到了你$`),
		regexp.MustCompile(`^\S+ 在 (.+) 里回复了你$`),
		regexp.MustCompile(`感谢了你在主题 › (.+) 里的回复$`),
		regexp.MustCompile(`(?:感谢|收藏)了你发布的主题 › (.+)$`),
	}
)

func init() {
	for _, name := range []string{"title", "id", "published", "updated", "author", "name", "content"} {
		feedElems[name] = regexp.MustCompile(`(?i)<` + name + `(?:\s[^>]*)?>([\s\S]*?)</` + name + `>`)
	}
}

func elem(s, name string) (string, bool) {
	m := feedElems[name].FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func unwrapCDATA(s string) string {
	if m := feedCDATA.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return decodeEntities(s)
}

// topicTitle：空标题（感谢或收藏）返回 nil。
func topicTitle(title string) *string {
	for _, p := range feedTitles {
		if m := p.FindStringSubmatch(title); m != nil {
			t := strings.TrimSpace(m[1])
			return &t
		}
	}
	return nil
}

func parseFeed(xml string) []Item {
	items := []Item{}
	for _, m := range feedEntry.FindAllStringSubmatch(xml, -1) {
		e := m[1]
		rawTitle, _ := elem(e, "title")
		title := strings.TrimSpace(decodeEntities(rawTitle))
		var href string
		if l := feedLink.FindStringSubmatch(e); l != nil {
			href = l[1]
		}
		rawID, _ := elem(e, "id")
		entryID := strings.TrimSpace(decodeEntities(rawID))
		published, ok := elem(e, "published")
		if !ok {
			published, _ = elem(e, "updated")
		}
		published = strings.TrimSpace(published)
		authorBlock, _ := elem(e, "author")
		rawAuthor, _ := elem(authorBlock, "name")
		author := strings.TrimSpace(decodeEntities(rawAuthor))
		t, err := time.Parse(time.RFC3339, published)
		if entryID == "" || err != nil {
			continue
		}
		typ := TypeOther
		var ttl *string
		if title != "" {
			typ = classify(title)
			ttl = topicTitle(title)
		}
		topic, anchor := parseTopicLink(href)
		content, _ := elem(e, "content")
		items = append(items, Item{
			ID:    entryID + "|" + published + "|" + author,
			T:     t.Unix(),
			Type:  typ,
			U:     author,
			Topic: topic,
			Floor: floorFor(typ, anchor),
			TTL:   ttl,
			Body:  htmlToText(unwrapCDATA(content)),
			Raw:   title,
		})
	}
	return items
}

func (p *Poller) fetchFeed(feedURL, etag string) fetchResult {
	res, err := p.getV2EX(feedURL, etag, "")
	if err != nil {
		return fetchFailed(netDetail(err))
	}
	defer res.Body.Close()
	remaining, resetAt := rateInfo(res.Header)
	switch {
	case res.StatusCode == http.StatusNotModified:
		return fetchResult{kind: fetchNotModified, remaining: remaining, resetAt: resetAt}
	case res.StatusCode == http.StatusNotFound:
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
	// 令牌不存在（被「重新生成 Token」作废）时，V2EX 返回 200 和空正文，不是 404。
	if len(bytes.TrimSpace(data)) == 0 {
		return fetchResult{kind: fetchInvalid, remaining: -1, resetAt: -1, empty: true}
	}
	if !bytes.Contains(data, []byte("<feed")) {
		return fetchFailed("响应不是 Atom Feed")
	}
	return fetchResult{kind: fetchOK, etag: res.Header.Get("Etag"), items: parseFeed(string(data)), remaining: remaining, resetAt: resetAt}
}
