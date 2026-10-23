package notifier

// 请求 V2EX 与中继共用：发请求、读完正文；网络层出错时换新连接重发一次。

import (
	"io"
	"net/http"
)

// response 是读完正文的响应。
type response struct {
	status int
	header http.Header
	body   []byte
}

// sender 发 HTTP 请求。所有请求共用 client 的连接池，去 V2EX 的请求复用同一条长连接（实测一条用两小时以上）。
// 这条连接在途中被代理或网络切断时，正在进行的请求得到 EOF 或 unexpected EOF，下一次请求新建连接就恢复。
// 所以没拿到完整响应时立即换新连接重发一次，不把一次断线算成一次失败。
type sender struct {
	client *http.Client
	log    Logger
}

// send 发请求并读完正文（最多 limit 字节）。
// 没拿到完整响应（连接被断开、重置、拒绝，或超时）时，先关掉空闲连接：断掉的那条 net/http 不会再用，
// 但代理重启、网络切换时同一时刻断开的往往不止一条。replay 为真时再用新连接立即重发一次，只重发一次。
// 拿到完整响应就不重发，5xx 也一样，由调用方按状态码处理。
func (s sender) send(req *http.Request, limit int64, replay bool) (response, error) {
	res, err := s.once(req, limit)
	if err == nil {
		return res, nil
	}
	s.client.CloseIdleConnections()
	if !replay {
		return res, err
	}
	again, rerr := rewind(req)
	if rerr != nil {
		return res, err
	}
	s.log.Infof("请求 %s 时网络出错（%s），已换新连接重试", req.URL.Host, netDetail(err))
	return s.once(again, limit)
}

func (s sender) once(req *http.Request, limit int64) (response, error) {
	res, err := s.client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, limit))
	if err != nil {
		return response{}, err // 正文读到一半连接断了，和没拿到响应一样处理
	}
	return response{status: res.StatusCode, header: res.Header, body: body}, nil
}

// rewind 复制请求用于重发。有正文时用 GetBody 重新取一份（http.NewRequest 传 bytes.Reader 时会设好）。
func rewind(req *http.Request) (*http.Request, error) {
	again := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		again.Body = body
	}
	return again, nil
}
