package notifier

// 轮询程序调用的三个中继接口：拉配置、发推送、心跳。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

type relayErrKind int

const (
	relayRevoked relayErrKind = iota + 1 // 推送 key 已失效（用户在 App 里重置了）
	relayPayment                         // 订阅无效，中继暂停转发
	relayRetry                           // 暂时不可用，稍后重试
)

type relayError struct {
	kind   relayErrKind
	after  time.Duration // Retry-After，没有时为 0
	detail string
}

type relayClient struct {
	base, pushKey, userAgent string
	sender
}

// call 发请求并读完正文。401、402、429、5xx 与网络错误归为 relayError。
// replay：网络层出错时是否换新连接立即重发一次，只给重发没有副作用的请求。
func (r *relayClient) call(method, path string, body any, ifNoneMatch string, replay bool) (int, []byte, *relayError) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(marshal(body))
	}
	req, err := http.NewRequest(method, r.base+path, rd)
	if err != nil {
		return 0, nil, &relayError{kind: relayRetry, detail: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+r.pushKey)
	req.Header.Set("User-Agent", r.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	res, err := r.send(req, 64<<10, replay)
	if err != nil {
		return 0, nil, &relayError{kind: relayRetry, detail: netDetail(err)}
	}
	switch {
	case res.status == http.StatusUnauthorized:
		return 0, nil, &relayError{kind: relayRevoked}
	case res.status == http.StatusPaymentRequired:
		return 0, nil, &relayError{kind: relayPayment}
	case res.status == http.StatusTooManyRequests || res.status >= 500:
		e := &relayError{kind: relayRetry, detail: fmt.Sprintf("HTTP %d", res.status)}
		if s, err := strconv.Atoi(res.header.Get("Retry-After")); err == nil && s > 0 {
			e.after = time.Duration(min(s, 3600)) * time.Second // 最多等一小时
		}
		return 0, nil, e
	}
	return res.status, res.body, nil
}

type configReply struct {
	notModified bool
	none        bool // 中继上还没有配置
	v           int
	blob        string
}

// getConfig：have 是已应用的配置版本，0 表示没有。
func (r *relayClient) getConfig(have int) (configReply, *relayError) {
	etag := ""
	if have > 0 {
		etag = fmt.Sprintf(`"%d"`, have)
	}
	status, data, rerr := r.call(http.MethodGet, "/v1/config", nil, etag, true)
	switch {
	case rerr != nil:
		return configReply{}, rerr
	case status == http.StatusNotModified:
		return configReply{notModified: true}, nil
	case status == http.StatusNotFound:
		return configReply{none: true}, nil
	case status/100 != 2:
		return configReply{}, &relayError{kind: relayRetry, detail: fmt.Sprintf("HTTP %d", status)}
	}
	var body struct {
		V    *int    `json:"v"`
		Blob *string `json:"blob"`
	}
	if json.Unmarshal(data, &body) != nil || body.V == nil || body.Blob == nil {
		return configReply{}, &relayError{kind: relayRetry, detail: "配置响应格式不对"}
	}
	return configReply{v: *body.V, blob: *body.Blob}, nil
}

// push 返回非空 rejected 表示中继明确拒收这一条（如 413），重试也没用，应丢弃。
// 网络出错时不立即重发：请求可能已经到了中继、只是回复丢了，而中继不去重，重发会让用户收到两条。
// 这条留在待发队列里，约 1 分钟后照常再发（flush）；即时重发只快这一点，不值得多开一条会重复的路。
func (r *relayClient) push(blob string) (rejected string, rerr *relayError) {
	status, _, rerr := r.call(http.MethodPost, "/v1/push", map[string]string{"blob": blob}, "", false)
	switch {
	case rerr != nil:
		return "", rerr
	case status >= 400:
		return fmt.Sprintf("HTTP %d", status), nil
	case status/100 != 2:
		return "", &relayError{kind: relayRetry, detail: fmt.Sprintf("HTTP %d", status)}
	}
	return "", nil
}

type heartbeatBody struct {
	Ver   string `json:"ver"`
	Cfg   *int   `json:"cfg"`
	State string `json:"state"`
	Last  *int64 `json:"last"`
	// 实例 id 与加密的机器信息，同一部署串多处运行时 App 据此提示。
	Inst string `json:"inst,omitempty"`
	Meta string `json:"meta,omitempty"`
}

// heartbeat 返回中继上的最新配置版本，没有时为 0。
// 网络出错时立即重发：重复的心跳只是再记一次同样的状态，「已连接」提醒中继只发一次。
func (r *relayClient) heartbeat(hb heartbeatBody) (int, *relayError) {
	status, data, rerr := r.call(http.MethodPost, "/v1/heartbeat", hb, "", true)
	if rerr != nil {
		return 0, rerr
	}
	if status/100 != 2 {
		return 0, &relayError{kind: relayRetry, detail: fmt.Sprintf("HTTP %d", status)}
	}
	var body struct {
		Cfg *int `json:"cfg"`
	}
	if json.Unmarshal(data, &body) != nil || body.Cfg == nil {
		return 0, nil
	}
	return *body.Cfg, nil
}
