package notifier

// 测试用的假世界：可控时钟、假中继、假 V2EX（API v2 与提醒 Feed）。样本数据全部编造。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	testRelay   = "https://relay.test"
	testFeedURL = "https://www.v2ex.com/n/abcdef0123456789abcdef0123456789abcdef01.xml"
)

func testDeploy() Deploy {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	pushKey := make([]byte, 16)
	for i := range pushKey {
		pushKey[i] = 7
	}
	return Deploy{Relay: testRelay, PushKey: b64.EncodeToString(pushKey), EncKey: key}
}

// buildDeployString 是 App 侧生成部署串的参考实现。
func buildDeployString(d Deploy) string {
	j, _ := json.Marshal(map[string]string{"r": d.Relay, "k": d.PushKey, "e": b64.EncodeToString(d.EncKey)})
	return deployPrefix + b64.EncodeToString(j)
}

type testNotification struct {
	ID       int64  `json:"id"`
	MemberID int64  `json:"member_id"`
	Text     string `json:"text"`
	Payload  string `json:"payload"`
	Created  int64  `json:"created"`
	Member   struct {
		Username string `json:"username"`
	} `json:"member"`
}

func replyNotification(id, created int64, user string, topic, floor int) testNotification {
	n := testNotification{
		ID:       id,
		MemberID: 5000 + id,
		Text: fmt.Sprintf(`<a href="/member/%s" target="_blank"><strong>%s</strong></a> 在 <a href="/t/%d#reply%d" class="topic-link">测试帖子 &amp; 标题</a> 里回复了你`,
			user, user, topic, floor),
		Payload: fmt.Sprintf("@me 第 %d 条回复", id),
		Created: created,
	}
	n.Member.Username = user
	return n
}

func thankNotification(id, created int64) testNotification {
	n := testNotification{
		ID:       id,
		MemberID: 9,
		Text:     `<a href="/member/bob" target="_blank"><strong>bob</strong></a> 感谢了你在主题 › <a href="/t/200#reply41" class="topic-link">另一个帖子</a> 里的回复`,
		Payload:  "我被感谢的那条回复",
		Created:  created,
	}
	n.Member.Username = "bob"
	return n
}

type testEntry struct{ title, link, published, author, content string }

// feedXML 的结构照抄真实提醒 Feed（2026-09-29），内容编造。
func feedXML(entries ...testEntry) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<title>Unread notifications for someone</title>
<subtitle>V2EX</subtitle>
<updated>2026-09-29T00:00:00Z</updated>
`)
	for _, e := range entries {
		fmt.Fprintf(&b, `<entry>
	<title>%s</title>
	<link rel="alternate" type="text/html" href="%s" />
	<id>tag:www.v2ex.com,%s:%s</id>
	<published>%s</published>
	<updated>%s</updated>
	<author>
		<name>%s</name>
		<uri>https://www.v2ex.com/member/%s</uri>
	</author>
	<content type="html" xml:base="https://www.v2ex.com/" xml:lang="en"><![CDATA[
		%s
	]]></content>
</entry>
`, e.title, e.link, e.published[:10], strings.TrimPrefix(e.link, "https://www.v2ex.com"), e.published, e.published, e.author, e.author, e.content)
	}
	b.WriteString("</feed>")
	return b.String()
}

type world struct {
	t      *testing.T
	now    time.Time
	deploy Deploy
	box    *Box
	// 中继
	config      *SavedConfig
	pushes      []string
	heartbeats  []heartbeatBody
	relayDown   bool
	relayStatus int // 非 0 时所有中继请求都返回该状态码
	// V2EX
	notifications  []testNotification
	apiStatus      int
	rateRemaining  int
	tokenExpiresAt int64
	feed           string
	feedStatus     int
	// 记录
	requests []string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, now: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC), deploy: testDeploy(), rateRemaining: 590}
	w.tokenExpiresAt = w.sec() + 180*86400
	box, err := NewBox(w.deploy.EncKey)
	if err != nil {
		t.Fatal(err)
	}
	w.box = box
	return w
}

func (w *world) sec() int64 { return w.now.Unix() }

func (w *world) advance(d time.Duration) { w.now = w.now.Add(d) }

func (w *world) setConfig(cfg map[string]any) {
	j, _ := json.Marshal(cfg)
	w.config = &SavedConfig{V: cfg["v"].(int), Blob: w.box.Seal(PurposeConfig, j)}
}

func (w *world) openedPushes() []map[string]any {
	w.t.Helper()
	out := []map[string]any{}
	for _, blob := range w.pushes {
		plain, err := w.box.Open(PurposePush, blob)
		if err != nil {
			w.t.Fatalf("推送解不开：%v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(plain, &m); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func (w *world) lastHeartbeat() heartbeatBody {
	w.t.Helper()
	if len(w.heartbeats) == 0 {
		w.t.Fatal("还没有心跳")
	}
	return w.heartbeats[len(w.heartbeats)-1]
}

func (w *world) v2exRequests() int {
	n := 0
	for _, r := range w.requests {
		if strings.Contains(r, "v2ex.com") {
			n++
		}
	}
	return n
}

func respond(req *http.Request, status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func jsonBody(v any) string {
	j, _ := json.Marshal(v)
	return string(j)
}

func (w *world) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL.String()
	w.requests = append(w.requests, req.Method+" "+u)
	switch {
	case strings.HasPrefix(u, testRelay):
		return w.relayResponse(req, strings.TrimPrefix(u, testRelay))
	case strings.HasPrefix(u, apiBase+"/"):
		return w.apiResponse(req), nil
	case strings.HasPrefix(u, "https://www.v2ex.com/n/"):
		return w.feedResponse(req), nil
	}
	w.t.Fatalf("意外的请求：%s", u)
	return nil, nil
}

func (w *world) relayResponse(req *http.Request, path string) (*http.Response, error) {
	if w.relayDown {
		return nil, fmt.Errorf("connection refused")
	}
	if w.relayStatus != 0 {
		return respond(req, w.relayStatus, nil, "{}"), nil
	}
	if req.Header.Get("Authorization") != "Bearer "+w.deploy.PushKey {
		return respond(req, 401, nil, "{}"), nil
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	switch path {
	case "/v1/config":
		if w.config == nil {
			return respond(req, 404, nil, "{}"), nil
		}
		if req.Header.Get("If-None-Match") == fmt.Sprintf(`"%d"`, w.config.V) {
			return respond(req, 304, nil, ""), nil
		}
		return respond(req, 200, nil, jsonBody(w.config)), nil
	case "/v1/push":
		var b struct{ Blob string }
		json.Unmarshal(body, &b)
		w.pushes = append(w.pushes, b.Blob)
		return respond(req, 202, nil, "{}"), nil
	case "/v1/heartbeat":
		var hb heartbeatBody
		json.Unmarshal(body, &hb)
		w.heartbeats = append(w.heartbeats, hb)
		var cfg *int
		if w.config != nil {
			cfg = &w.config.V
		}
		return respond(req, 200, nil, jsonBody(map[string]*int{"cfg": cfg})), nil
	}
	return respond(req, 404, nil, "{}"), nil
}

func (w *world) apiResponse(req *http.Request) *http.Response {
	h := http.Header{}
	h.Set("X-Rate-Limit-Remaining", fmt.Sprint(w.rateRemaining))
	h.Set("X-Rate-Limit-Reset", fmt.Sprint(w.sec()+3600))
	if w.apiStatus != 0 {
		return respond(req, w.apiStatus, h, "{}")
	}
	if strings.HasSuffix(req.URL.Path, "/token") {
		return respond(req, 200, nil, jsonBody(map[string]any{
			"success": true, "result": map[string]int64{"created": w.tokenExpiresAt - 15552000, "expiration": 15552000},
		}))
	}
	list := append([]testNotification{}, w.notifications...)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Created > list[j].Created })
	list = list[:min(10, len(list))]
	ids := make([]string, len(list))
	for i, n := range list {
		ids[i] = fmt.Sprint(n.ID)
	}
	etag := fmt.Sprintf(`W/"%s"`, strings.Join(ids, "-"))
	if req.Header.Get("If-None-Match") == etag {
		return respond(req, 304, h, "")
	}
	h.Set("Etag", etag)
	return respond(req, 200, h, jsonBody(map[string]any{"success": true, "message": "", "result": list}))
}

func (w *world) feedResponse(req *http.Request) *http.Response {
	if w.feedStatus != 0 {
		return respond(req, w.feedStatus, nil, "not found")
	}
	etag := fmt.Sprintf(`W/"%d"`, len(w.feed))
	if req.Header.Get("If-None-Match") == etag {
		return respond(req, 304, nil, "")
	}
	h := http.Header{}
	h.Set("Etag", etag)
	h.Set("Content-Type", "application/atom+xml;charset=UTF-8")
	return respond(req, 200, h, w.feed)
}

// memoryStore 用 JSON 往返模拟落盘，保证测试里的状态和真实文件一样经过序列化。
type memoryStore struct {
	data  []byte
	saves int
}

func (m *memoryStore) Load() (*State, error) {
	var s State
	if m.data != nil {
		if err := json.Unmarshal(m.data, &s); err != nil {
			return &State{}, err
		}
	}
	return &s, nil
}

func (m *memoryStore) Save(s *State) error {
	m.data, _ = json.Marshal(s)
	m.saves++
	return nil
}

func (m *memoryStore) state() *State {
	s, _ := m.Load()
	return s
}

type testLogger struct{ t *testing.T }

func (l testLogger) Infof(f string, a ...any) { l.t.Logf(f, a...) }
func (l testLogger) Warnf(f string, a ...any) { l.t.Logf("[warn] "+f, a...) }

func (w *world) newPoller(store Store) *Poller {
	w.t.Helper()
	p, err := New(Options{
		Deploy:   w.deploy,
		Store:    store,
		Client:   &http.Client{Transport: w},
		Version:  "test",
		Now:      func() time.Time { return w.now },
		Log:      testLogger{w.t},
		Hostname: "nas-01",
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}
