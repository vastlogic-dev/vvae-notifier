package notifier

// HTML 转纯文本与 V2EX 动作文案分类。分类只用于按类型过滤，失败时返回 unknown，不影响推送。

import (
	"regexp"
	"strconv"
	"strings"
)

type ItemType string

const (
	TypeReply      ItemType = "reply"
	TypeMention    ItemType = "mention"
	TypeThankReply ItemType = "thank_reply"
	TypeThankTopic ItemType = "thank_topic"
	TypeFavorite   ItemType = "favorite"
	TypeOther      ItemType = "other"
	TypeUnknown    ItemType = "unknown"
)

var AllTypes = []ItemType{TypeReply, TypeMention, TypeThankReply, TypeThankTopic, TypeFavorite, TypeOther, TypeUnknown}

var (
	entityPattern = regexp.MustCompile(`(?i)&(#x[0-9a-f]+|#\d+|[a-z]+);`)
	namedEntities = map[string]string{"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "apos": "'", "nbsp": " "}

	brPattern      = regexp.MustCompile(`(?i)<br\s*/?>`)
	pEndPattern    = regexp.MustCompile(`(?i)</p>`)
	htmlTagPattern = regexp.MustCompile(`<[^>]+>`)
	spacesPattern  = regexp.MustCompile(`[ \t\r\f\v]+`)
	blankLines     = regexp.MustCompile(`\n{3,}`)

	topicLinkPattern = regexp.MustCompile(`/t/(\d+)(?:#reply(\d+))?`)
)

func decodeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	return entityPattern.ReplaceAllStringFunc(s, func(m string) string {
		e := m[1 : len(m)-1]
		if e[0] == '#' {
			var cp int64
			var err error
			if e[1] == 'x' || e[1] == 'X' {
				cp, err = strconv.ParseInt(e[2:], 16, 32)
			} else {
				cp, err = strconv.ParseInt(e[1:], 10, 32)
			}
			if err != nil || cp <= 0 || cp > 0x10ffff {
				return m
			}
			return string(rune(cp))
		}
		if r, ok := namedEntities[strings.ToLower(e)]; ok {
			return r
		}
		return m
	})
}

func htmlToText(html string) string {
	s := brPattern.ReplaceAllString(html, "\n")
	s = pEndPattern.ReplaceAllString(s, "\n")
	s = htmlTagPattern.ReplaceAllString(s, "")
	lines := strings.Split(decodeEntities(s), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(spacesPattern.ReplaceAllString(line, " "))
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// classify 按中文动作文案分类；请求带 Accept-Language: zh-CN 保证文案是中文。
func classify(action string) ItemType {
	switch {
	case strings.Contains(action, "时提到了你"):
		return TypeMention
	case strings.Contains(action, "里回复了你"):
		return TypeReply
	case strings.Contains(action, "感谢了你在主题"):
		return TypeThankReply
	case strings.Contains(action, "感谢了你发布的主题"):
		return TypeThankTopic
	case strings.Contains(action, "收藏了你发布的主题"):
		return TypeFavorite
	}
	return TypeUnknown
}

// parseTopicLink 从 /t/<id>#reply<N> 链接取帖子 id 和锚点数字，没有时为 nil。
func parseTopicLink(href string) (topic, anchor *int64) {
	m := topicLinkPattern.FindStringSubmatch(href)
	if m == nil {
		return nil, nil
	}
	topic = parseInt(m[1])
	if m[2] != "" {
		anchor = parseInt(m[2])
	}
	return topic, anchor
}

// floorFor：楼层号只对回复、提到有意义；感谢、收藏的锚点是当时的回复数，不是楼层。
func floorFor(t ItemType, anchor *int64) *int64 {
	if t == TypeReply || t == TypeMention {
		return anchor
	}
	return nil
}

func parseInt(s string) *int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}
