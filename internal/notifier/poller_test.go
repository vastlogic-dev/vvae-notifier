package notifier

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

var apiCfg = map[string]any{"v": 1, "source": "api", "pat": "pat-0000-1111"}

func with(base map[string]any, kv ...any) map[string]any {
	m := map[string]any{}
	for k, v := range base {
		m[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// setup 建假世界和轮询程序；run 推进时间并跑到期的任务。
func setup(t *testing.T, cfg map[string]any) (*world, *memoryStore, func(time.Duration) time.Duration) {
	w := newWorld(t)
	if cfg != nil {
		w.setConfig(cfg)
	}
	store := &memoryStore{}
	p := w.newPoller(store)
	return w, store, func(d time.Duration) time.Duration {
		w.advance(d)
		return p.Tick()
	}
}

func field(m map[string]any, keys ...string) string {
	out := ""
	for _, k := range keys {
		out += fmt.Sprintf("%s=%v ", k, m[k])
	}
	return out
}

func TestNoConfigThenBaseline(t *testing.T) {
	w, _, run := setup(t, nil)
	w.notifications = []testNotification{replyNotification(1, w.sec()-100, "alice", 100, 3)}
	run(0)
	if hb := w.lastHeartbeat(); hb.State != statusNoConfig {
		t.Fatalf("没有配置时以 no_config 心跳：%+v", hb)
	}
	w.setConfig(apiCfg)
	run(time.Minute)
	if hb := w.lastHeartbeat(); hb.Cfg != nil {
		t.Fatal("没有配置时 5 分钟后才重拉")
	}
	run(4 * time.Minute) // 重拉配置 → 基线
	if len(w.pushes) != 0 {
		t.Fatal("基线不推历史")
	}
	if hb := w.lastHeartbeat(); hb.Cfg == nil || *hb.Cfg != 1 || hb.State != statusOK {
		t.Fatalf("心跳上报已应用的配置版本：%+v", hb)
	}
}

func TestHeartbeatCarriesInstance(t *testing.T) {
	w, _, run := setup(t, nil)
	run(0)
	first := w.lastHeartbeat()
	if len(first.Inst) != 22 {
		t.Fatalf("实例 id 应是 16 字节 base64url：%q", first.Inst)
	}
	box, _ := NewBox(w.deploy.EncKey)
	plain, err := box.Open(PurposeInstance, first.Meta)
	if err != nil {
		t.Fatalf("机器信息应能用加密密钥解开：%v", err)
	}
	var meta struct {
		N string `json:"n"`
		A string `json:"a"`
		T int64  `json:"t"`
	}
	if json.Unmarshal(plain, &meta) != nil || meta.N != "nas-01" || meta.A == "" || meta.T != w.sec() {
		t.Fatalf("机器信息：%s", plain)
	}
	if strings.Contains(first.Meta, "nas-01") {
		t.Fatal("主机名不能明文出现")
	}
	run(5 * time.Minute)
	if hb := w.lastHeartbeat(); hb.Inst != first.Inst {
		t.Fatal("同一进程的实例 id 不变")
	}
}

func TestPushOnceAndNotModified(t *testing.T) {
	w, _, run := setup(t, apiCfg)
	w.notifications = []testNotification{replyNotification(1, w.sec()-100, "alice", 100, 3)}
	run(0) // 基线
	w.notifications = append(w.notifications, replyNotification(2, w.sec()+10, "alice", 100, 7))
	run(30 * time.Second)
	run(30 * time.Second) // 同一批数据，304
	pushed := w.openedPushes()
	if len(pushed) != 1 {
		t.Fatalf("推送 %d 条", len(pushed))
	}
	got := field(pushed[0], "kind", "src", "id", "type", "topic", "floor", "u")
	if want := "kind=n src=api id=2 type=reply topic=100 floor=7 u=alice "; got != want {
		t.Fatalf("载荷：%s", got)
	}
}

func TestTypeFilter(t *testing.T) {
	w, _, run := setup(t, with(apiCfg, "types", []string{"reply", "mention"}))
	run(0) // 基线（空）
	w.notifications = []testNotification{thankNotification(3, w.sec()+5), replyNotification(4, w.sec()+6, "alice", 100, 3)}
	run(30 * time.Second)
	pushed := w.openedPushes()
	if len(pushed) != 1 || pushed[0]["id"] != "4" {
		t.Fatalf("只推回复：%v", pushed)
	}
}

func TestTokenInvalidOnceThenRecover(t *testing.T) {
	w, _, run := setup(t, apiCfg)
	run(0)
	w.apiStatus = 401
	run(30 * time.Second)
	run(5 * time.Minute)
	run(5 * time.Minute)
	pushed := w.openedPushes()
	if len(pushed) != 1 || pushed[0]["code"] != "token_invalid" {
		t.Fatalf("401 只提醒一次：%v", pushed)
	}
	if hb := w.lastHeartbeat(); hb.State != statusTokenInvalid {
		t.Fatalf("心跳状态：%s", hb.State)
	}

	w.apiStatus = 0
	w.notifications = []testNotification{replyNotification(9, w.sec()-10, "alice", 100, 3)}
	w.setConfig(map[string]any{"v": 2, "source": "api", "pat": "pat-new-2222"})
	run(5 * time.Minute) // 心跳发现新版本
	run(time.Second)     // 拉配置 → 新凭据 → 重记基线
	if n := len(w.openedPushes()); n != 1 {
		t.Fatalf("新凭据的历史不推：%d", n)
	}
	if hb := w.lastHeartbeat(); hb.State != statusOK {
		t.Fatalf("恢复后心跳状态：%s", hb.State)
	}
}

func TestFeedInvalidOnEmptyBody(t *testing.T) {
	w, _, run := setup(t, map[string]any{"v": 1, "source": "feed", "feedURL": testFeedURL})
	w.feed = feedXML()
	run(0)
	w.feed = "" // V2EX 对作废的令牌返回 200 空正文
	run(time.Minute)
	run(2 * time.Minute)
	if len(w.pushes) != 0 {
		t.Fatal("空正文要连续确认，不能一次就判失效")
	}
	for range 15 {
		run(time.Minute)
	}
	pushed := w.openedPushes()
	if len(pushed) != 1 || pushed[0]["code"] != "feed_invalid" {
		t.Fatalf("空正文判为提醒源失效，只提醒一次：%v", pushed)
	}
	if hb := w.lastHeartbeat(); hb.State != statusFeedInvalid {
		t.Fatalf("心跳状态：%s", hb.State)
	}
}

func TestSingleEmptyFeedIsNotInvalid(t *testing.T) {
	w, _, run := setup(t, map[string]any{"v": 1, "source": "feed", "feedURL": testFeedURL})
	w.feed = feedXML()
	run(0)
	for range 5 { // 空正文与正常响应交替：每次恢复都清零，不推系统提醒
		w.feed = ""
		run(time.Minute)
		w.feed = feedXML()
		run(5 * time.Minute)
	}
	if len(w.pushes) != 0 {
		t.Fatalf("偶发空正文不能判失效：%v", w.openedPushes())
	}
}

func TestTokenExpiringOncePerTier(t *testing.T) {
	w, _, run := setup(t, apiCfg)
	w.tokenExpiresAt = w.sec() + 5*86400 + 3600
	run(0)
	run(day / 2)
	days := func() []any {
		var out []any
		for _, p := range w.openedPushes() {
			if p["kind"] == "s" {
				out = append(out, p["days"])
			}
		}
		return out
	}
	if got := fmt.Sprint(days()); got != "[5]" {
		t.Fatalf("剩 5 天只提醒一次：%s", got)
	}
	w.advance(4*day + day/2) // 剩约 1 小时
	run(0)
	run(day)
	if got := fmt.Sprint(days()); got != "[5 0]" {
		t.Fatalf("剩 1 天那一档再提醒一次：%s", got)
	}
}

func TestFeedURLUsedVerbatim(t *testing.T) {
	w, _, run := setup(t, map[string]any{"v": 1, "source": "feed", "feedURL": testFeedURL})
	w.feed = feedXML()
	run(0)
	published := w.now.Add(5 * time.Second).Format(time.RFC3339)
	w.feed = feedXML(testEntry{"erin 在 某帖 里回复了你", "https://www.v2ex.com/t/400#reply2", published, "erin", "hi"})
	run(time.Minute)
	feedReqs := 0
	for _, r := range w.requests {
		if strings.Contains(r, "/n/") {
			if r != "GET "+testFeedURL {
				t.Fatalf("提醒源地址被改动：%s", r)
			}
			feedReqs++
		}
	}
	if feedReqs < 2 {
		t.Fatalf("Feed 请求 %d 次", feedReqs)
	}
	pushed := w.openedPushes()
	if len(pushed) != 1 || pushed[0]["src"] != "feed" || pushed[0]["type"] != "reply" {
		t.Fatalf("Feed 推送：%v", pushed)
	}
}

func TestOutboxSurvivesRelayOutage(t *testing.T) {
	w, store, run := setup(t, apiCfg)
	run(0)
	w.relayDown = true
	w.notifications = []testNotification{replyNotification(5, w.sec()+1, "alice", 100, 3)}
	run(30 * time.Second)
	if n := len(store.state().Outbox); n != 1 {
		t.Fatalf("中继不可达时暂存：%d", n)
	}
	w.relayDown = false
	run(time.Minute)
	if len(w.pushes) != 1 || len(store.state().Outbox) != 0 {
		t.Fatalf("恢复后补发：pushes=%d outbox=%d", len(w.pushes), len(store.state().Outbox))
	}
}

func TestRevokedKeyPausesEverything(t *testing.T) {
	w, _, run := setup(t, apiCfg)
	run(0)
	w.relayStatus = 401
	run(5 * time.Minute)
	before := w.v2exRequests()
	if delay := run(time.Minute); delay < 20*time.Minute {
		t.Fatalf("暂停时长 %v", delay)
	}
	if w.v2exRequests() != before {
		t.Fatal("推送 key 失效后不再请求 V2EX")
	}
}

func TestRestartResumesWithoutDuplicates(t *testing.T) {
	w, store, _ := setup(t, apiCfg)
	w.newPoller(store).Tick()
	w.notifications = []testNotification{replyNotification(6, w.sec()+1, "alice", 100, 3)}
	w.advance(30 * time.Second)
	w.newPoller(store).Tick()
	w.advance(30 * time.Second)
	w.newPoller(store).Tick() // 新进程：配置从本地密文恢复，seen 已含 6
	if len(w.pushes) != 1 {
		t.Fatalf("重启后重复推送：%d", len(w.pushes))
	}
}

func TestInstanceSurvivesRestart(t *testing.T) {
	w, store, run := setup(t, apiCfg)
	run(0)
	first := w.lastHeartbeat().Inst
	w.advance(time.Minute)
	w.newPoller(store).Tick() // 新进程
	if hb := w.lastHeartbeat(); hb.Inst != first {
		t.Fatalf("重启后实例 id 变了：%q → %q", first, hb.Inst)
	}
	w.setConfig(with(apiCfg, "v", 2, "pat", "pat-2222-3333")) // 换凭据：清状态、重新记基线
	run(5 * time.Minute)
	run(time.Second)
	if s := store.state(); s.Inst != first || s.Cfg == nil || s.Cfg.V != 2 {
		t.Fatalf("换凭据后实例 id 应保留：%q，配置 %+v", s.Inst, s.Cfg)
	}
}

func TestSharedAPIBudget(t *testing.T) {
	// 一小时内请求 V2EX 的次数：一个部署串按配置的 30 秒；5 个共用限额时每个拉长到 43 秒，合计不超过 600 的 70%。
	perHour := func(share int) int {
		w := newWorld(t)
		w.setConfig(apiCfg)
		p := w.newPollerSharing(&memoryStore{}, share)
		p.Tick()
		before := w.v2exRequests()
		for end := w.now.Add(time.Hour); w.now.Before(end); {
			w.advance(p.Tick())
		}
		return w.v2exRequests() - before
	}
	if n := perHour(1); n < 118 || n > 122 {
		t.Fatalf("单个部署串一小时 %d 次，应约 120", n)
	}
	if n := perHour(5); n*5 > apiHourlyBudget || n < 80 {
		t.Fatalf("5 个部署串时每个一小时 %d 次，合计应不超过 %d", n, apiHourlyBudget)
	}
}

func TestBaselineIgnoresFastLocalClock(t *testing.T) {
	// 本机时钟比 V2EX 快：记基线之后 V2EX 上新产生的提醒，时间仍早于本机的「现在」，也要推送。
	w, _, run := setup(t, apiCfg)
	w.notifications = []testNotification{replyNotification(1, w.sec()-600, "alice", 100, 3)}
	run(0)
	w.notifications = append([]testNotification{replyNotification(2, w.sec()-300, "bob", 100, 4)}, w.notifications...)
	run(30 * time.Second)
	if len(w.pushes) != 1 {
		t.Fatalf("基线后的新提醒被当成旧的：推送 %d 条", len(w.pushes))
	}
}

func TestRejectedPushDropped(t *testing.T) {
	w, store, run := setup(t, apiCfg)
	run(0)
	w.relayStatus = 413
	w.notifications = []testNotification{replyNotification(7, w.sec()+1, "alice", 100, 3)}
	run(30 * time.Second)
	if n := len(store.state().Outbox); n != 0 {
		t.Fatalf("中继拒收的推送要丢弃：%d", n)
	}
}

func TestPaymentRequiredDropsOutbox(t *testing.T) {
	w, store, run := setup(t, apiCfg)
	run(0)
	w.relayStatus = 402
	w.notifications = []testNotification{replyNotification(8, w.sec()+1, "alice", 100, 3)}
	run(30 * time.Second)
	if n := len(store.state().Outbox); n != 0 {
		t.Fatalf("订阅无效时丢弃待发推送：%d", n)
	}
}
