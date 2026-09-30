package notifier

// 两种轮询源共用的部分：归一后的提醒、拉取结果、请求 V2EX。

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Item 是两种轮询源归一后的提醒。
type Item struct {
	ID    string
	T     int64
	Type  ItemType
	U     string
	MID   *int64
	Topic *int64
	Floor *int64
	TTL   *string
	Body  string
	Raw   string
}

type fetchKind int

const (
	fetchOK fetchKind = iota
	fetchNotModified
	fetchInvalid // api 401 / feed 404 或空正文：凭据或地址失效
	fetchRateLimited
	fetchError
)

type fetchResult struct {
	kind      fetchKind
	etag      string
	items     []Item
	remaining int64 // -1 表示响应没带
	resetAt   int64 // Unix 秒，-1 表示响应没带
	detail    string
	empty     bool // fetchInvalid 的原因是 200 空正文，需要连续确认
}

func fetchFailed(detail string) fetchResult {
	return fetchResult{kind: fetchError, remaining: -1, resetAt: -1, detail: detail}
}

// 正文上限：提醒 Feed 约 33KB，通知接口约 8KB，远低于它。
const maxBody = 4 << 20

// getV2EX 发 GET。UA 自报，不伪装浏览器；分类依赖中文动作文案，所以固定请求中文。
func (p *Poller) getV2EX(rawURL, etag, bearer string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return p.client.Do(req)
}

func readBody(res *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(res.Body, maxBody))
}

func rateInfo(h http.Header) (remaining, resetAt int64) {
	num := func(name string) int64 {
		n, err := strconv.ParseUint(h.Get(name), 10, 63)
		if err != nil {
			return -1
		}
		return int64(n)
	}
	return num("X-Rate-Limit-Remaining"), num("X-Rate-Limit-Reset")
}

// netDetail 去掉 url.Error 里的地址：提醒源地址本身就是凭据，不进日志。
func netDetail(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "请求超时"
		}
		return ue.Err.Error()
	}
	return err.Error()
}
