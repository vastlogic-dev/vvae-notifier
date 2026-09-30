package notifier

// 轮询程序核心：Tick 执行到期的任务，返回距下一个任务的时长。

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	heartbeatEvery = 5 * time.Minute
	configRetry    = time.Minute
	// App 先上传配置再给出部署串，正常不会没有配置；真没有、或配置解不开时放慢，
	// 避免占用中继的拉配置限额。App 改了配置，心跳会立即发现。
	noConfigRetry   = 5 * time.Minute
	invalidRetry    = 5 * time.Minute
	maxBackoff      = 5 * time.Minute
	tokenCheckEvery = 24 * time.Hour
	tokenCheckRetry = time.Hour
	revokedRetry    = 30 * time.Minute
	lowRemaining    = 60
	// V2EX API 按 IP 限额每小时 600 次；同一进程的 API 轮询合计不超过它的 70%，给用户在同一 IP 上的其他用途留余量。
	apiHourlyBudget = 600 * 7 / 10
	// 提醒源连续这么多次返回空正文才判失效（间隔按错误退避，约 6 分钟，超过边缘缓存的 150 秒）。
	emptyConfirm = 3
)

// 心跳里上报的状态，App 据此显示轮询程序是否正常。
const (
	statusOK           = "ok"
	statusNoConfig     = "no_config"
	statusTokenInvalid = "token_invalid"
	statusFeedInvalid  = "feed_invalid"
	statusRateLimited  = "rate_limited"
	statusError        = "error"
)

type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Infof(string, ...any) {}
func (nopLogger) Warnf(string, ...any) {}

type Options struct {
	Deploy  Deploy
	Store   Store
	Client  *http.Client
	Version string
	Now     func() time.Time // 测试注入；缺省 time.Now
	Log     Logger
	// 主机名，随心跳加密上报，App 用来标出轮询程序在哪台机器上。容器里是容器的主机名。
	Hostname string
	// 同一进程里一起轮询的部署串数，共用一个出口 IP 的 V2EX API 限额；缺省 1。
	APIShare int
}

type Poller struct {
	store     Store
	client    *http.Client
	userAgent string
	version   string
	now       func() time.Time
	log       Logger
	box       *Box
	relay     *relayClient
	// 本进程的实例 id 与加密后的机器信息，启动时定下，每次心跳带上。
	inst, meta string
	// API 轮询间隔下限：几个部署串共用限额时拉长，合计不超过 apiHourlyBudget。
	apiFloor time.Duration

	state       *State
	config      *Config
	status      string
	lastOK      *int64 // 最近一次成功轮询，Unix 秒。只在内存里：重启后第一轮就会补上，不必每次轮询都写盘
	dirty       bool
	saveFailing bool

	// 时间表只在内存里，重启后立即各跑一次。
	nextPoll, nextHeartbeat, nextConfigPull, nextFlush time.Time
	wantConfig                                         bool
	errorStreak                                        int
	emptyStreak                                        int
	revokedUntil                                       time.Time
}

func New(o Options) (*Poller, error) {
	box, err := NewBox(o.Deploy.EncKey)
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = nopLogger{}
	}
	ua := "vvae-notifier/" + o.Version
	inst := make([]byte, 16)
	if _, err := rand.Read(inst); err != nil {
		return nil, err
	}
	p := &Poller{
		inst:       b64.EncodeToString(inst),
		meta:       instanceMeta(box, o.Hostname, o.Now()),
		store:      o.Store,
		client:     o.Client,
		userAgent:  ua,
		version:    o.Version,
		now:        o.Now,
		log:        o.Log,
		box:        box,
		relay:      &relayClient{base: o.Deploy.Relay, pushKey: o.Deploy.PushKey, userAgent: ua, client: o.Client},
		status:     statusNoConfig,
		wantConfig: true,
		apiFloor:   time.Duration((3600*max(o.APIShare, 1)+apiHourlyBudget-1)/apiHourlyBudget) * time.Second,
	}
	p.loadState()
	return p, nil
}

// loadState 读状态、恢复配置。实例 id 随状态保存：同一个部署重启后不变，
// App 不会把重启前的自己当成另一个在运行的轮询程序。状态里没有时用 New 随机的那个。
func (p *Poller) loadState() {
	st, err := p.store.Load()
	if err != nil {
		p.log.Warnf("读取状态失败，重新记基线：%v", err)
	}
	p.state = st
	if st.Inst == "" {
		st.Inst = p.inst
		p.dirty = true
	}
	p.inst = st.Inst
	p.restoreConfig()
}

func (p *Poller) Tick() time.Duration {
	now := p.now()
	if now.Before(p.revokedUntil) {
		return p.revokedUntil.Sub(now)
	}

	if p.wantConfig && !now.Before(p.nextConfigPull) {
		p.pullConfig()
	}
	if p.config != nil && !now.Before(p.nextPoll) {
		p.poll()
	}
	if p.config != nil && p.config.Source == SourceAPI && now.Unix() >= p.state.NextTokenCheck {
		p.checkToken()
	}
	if len(p.state.Outbox) > 0 && !p.now().Before(p.nextFlush) {
		p.flush()
		p.save() // 心跳可能卡到超时，先记下已发出的推送，被强杀后重启不重发
	}
	if !p.now().Before(p.nextHeartbeat) {
		p.heartbeat()
	}
	p.save()

	due := p.nextHeartbeat
	earlier := func(t time.Time) {
		if t.Before(due) {
			due = t
		}
	}
	if p.config != nil {
		earlier(p.nextPoll)
		if p.config.Source == SourceAPI {
			earlier(time.Unix(p.state.NextTokenCheck, 0))
		}
	}
	if p.wantConfig {
		earlier(p.nextConfigPull)
	}
	if len(p.state.Outbox) > 0 {
		earlier(p.nextFlush)
	}
	return max(time.Second, due.Sub(p.now()))
}

func (p *Poller) save() {
	if !p.dirty {
		return
	}
	if err := p.store.Save(p.state); err != nil {
		if !p.saveFailing {
			p.log.Warnf("状态保存失败，重启后会重新记基线：%v", err)
			p.saveFailing = true
		}
		return
	}
	p.dirty = false
	p.saveFailing = false
}

// ---- 配置 ----

func (p *Poller) openConfig(blob string) (*Config, error) {
	plain, err := p.box.Open(PurposeConfig, blob)
	if err != nil {
		return nil, err
	}
	return ParseConfig(plain)
}

func (p *Poller) restoreConfig() {
	saved := p.state.Cfg
	if saved == nil {
		return
	}
	cfg, err := p.openConfig(saved.Blob)
	if err != nil {
		p.log.Warnf("本地保存的配置无法解密，重新拉取：%v", err)
		p.state.Cfg = nil
		p.dirty = true
		return
	}
	p.config = cfg
	p.setStatus(statusOK)
}

func (p *Poller) appliedVersion() int {
	if p.config == nil {
		return 0
	}
	return p.config.V
}

func (p *Poller) pullConfig() {
	r, rerr := p.relay.getConfig(p.appliedVersion())
	if rerr != nil {
		p.handleRelayError(rerr)
		p.nextConfigPull = p.now().Add(configRetry)
		return
	}
	if r.none {
		p.setStatus(statusNoConfig)
		p.nextConfigPull = p.now().Add(noConfigRetry)
		return
	}
	if r.notModified {
		p.wantConfig = false
		return
	}
	cfg, err := p.openConfig(r.blob)
	if err != nil {
		p.log.Warnf("配置无法解密或格式不对，确认部署串与 App 一致：%v", err)
		p.setStatus(statusError)
		p.nextConfigPull = p.now().Add(noConfigRetry)
		return
	}
	p.wantConfig = false
	p.applyConfig(cfg, r.blob)
}

func (p *Poller) applyConfig(cfg *Config, blob string) {
	s := p.state
	if fp := cfg.sourceFingerprint(); fp != s.SourceKey {
		// 换了轮询源或凭据：重新记基线，清掉旧源的状态。
		*s = State{SourceKey: fp, Outbox: s.Outbox, Inst: s.Inst}
		p.errorStreak = 0
		p.nextPoll = time.Time{}
	}
	s.Cfg = &SavedConfig{V: cfg.V, Blob: blob}
	p.config = cfg
	if p.status == statusNoConfig {
		p.setStatus(statusOK)
	}
	p.nextHeartbeat = time.Time{} // 尽快上报已应用的配置版本
	p.dirty = true
	if iv := p.interval(cfg); iv > time.Duration(cfg.Interval)*time.Second {
		p.log.Infof("已应用配置 v%d（%s，间隔 %d 秒；几个账号共用 V2EX 限额，实际 %d 秒）", cfg.V, cfg.Source, cfg.Interval, int(iv.Seconds()))
	} else {
		p.log.Infof("已应用配置 v%d（%s，间隔 %d 秒）", cfg.V, cfg.Source, cfg.Interval)
	}
}

// interval 是实际的轮询间隔：配置的间隔，API 源不短于共用限额算出的下限。
func (p *Poller) interval(cfg *Config) time.Duration {
	iv := time.Duration(cfg.Interval) * time.Second
	if cfg.Source == SourceAPI {
		iv = max(iv, p.apiFloor)
	}
	return iv
}

// ---- 轮询 ----

func (p *Poller) poll() {
	cfg, s := p.config, p.state
	var r fetchResult
	if cfg.Source == SourceAPI {
		r = p.fetchNotifications(cfg.PAT, s.ETag)
	} else {
		r = p.fetchFeed(cfg.FeedURL, s.ETag)
	}
	now := p.now()
	interval := p.interval(cfg)
	next := now.Add(interval)

	// 空正文可能是边缘节点偶发的坏响应，单次出现先按错误退避，免得误报失效、来回推系统提醒。
	if r.kind == fetchInvalid && r.empty {
		if p.emptyStreak++; p.emptyStreak < emptyConfirm {
			r = fetchFailed("提醒源返回空内容")
		}
	}

	switch r.kind {
	case fetchOK:
		if r.etag != s.ETag {
			s.ETag = r.etag
			p.dirty = true
		}
		p.ingest(r.items)
		p.markHealthy(now)
	case fetchNotModified:
		p.markHealthy(now)
	case fetchInvalid:
		code, msg := statusTokenInvalid, "PAT 已失效（401）"
		if cfg.Source == SourceFeed {
			code, msg = statusFeedInvalid, "提醒源地址已失效"
		}
		p.setStatus(code)
		if !s.InvalidNotified {
			s.InvalidNotified = true
			p.enqueue(marshal(systemPayload{Kind: "s", Code: code}))
			p.log.Warnf("%s，已发提醒，等待 App 下发新配置", msg)
		}
		next = now.Add(invalidRetry)
		p.wantConfig = true // 等 App 下发新凭据
	case fetchRateLimited:
		p.setStatus(statusRateLimited)
		next = now.Add(maxBackoff)
		if r.resetAt > 0 {
			next = later(now.Add(time.Minute), time.Unix(r.resetAt, 0))
		}
		p.log.Warnf("V2EX 限流，暂停到限额重置")
	case fetchError:
		p.setStatus(statusError)
		p.errorStreak++
		next = now.Add(min(maxBackoff, interval<<min(p.errorStreak, 16)))
		p.log.Warnf("轮询失败：%s", r.detail)
	}
	if (r.kind == fetchOK || r.kind == fetchNotModified) && r.remaining >= 0 && r.remaining < lowRemaining && r.resetAt > 0 {
		next = later(next, time.Unix(r.resetAt, 0))
	}
	p.nextPoll = next
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (p *Poller) markHealthy(now time.Time) {
	p.setStatus(statusOK)
	p.errorStreak = 0
	p.emptyStreak = 0
	if p.state.InvalidNotified {
		p.state.InvalidNotified = false
		p.dirty = true
	}
	t := now.Unix()
	p.lastOK = &t
}

func (p *Poller) ingest(items []Item) {
	s, cfg := p.state, p.config
	if s.BaselineT == nil {
		// 基线取已有提醒里最新的时间，不看本机时钟：本机比 V2EX 快时，本机时间会把记基线后不久的新提醒当成旧的。
		// 列表为空才用本机时间，防止之后拉到的历史提醒被当成新的。
		var t int64
		if len(items) == 0 {
			t = p.now().Unix()
		}
		s.Seen = []string{}
		for _, it := range items {
			t = max(t, it.T)
			if len(s.Seen) < seenLimit {
				s.Seen = append(s.Seen, it.ID)
			}
		}
		s.BaselineT = &t
		p.dirty = true
		p.log.Infof("已记录基线（%d 条），之后的新提醒才推送", len(items))
		return
	}
	seen := make(map[string]bool, len(s.Seen)+len(items))
	for _, id := range s.Seen {
		seen[id] = true
	}
	var fresh []Item
	for _, it := range items {
		if !seen[it.ID] {
			seen[it.ID] = true
			fresh = append(fresh, it)
		}
	}
	if len(fresh) == 0 {
		return
	}
	// 从旧到新发送；seen 里新的在前。
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].T < fresh[j].T })
	ids := make([]string, 0, len(fresh)+len(s.Seen))
	for i := len(fresh) - 1; i >= 0; i-- {
		ids = append(ids, fresh[i].ID)
	}
	s.Seen = append(ids, s.Seen...)[:min(seenLimit, len(ids)+len(s.Seen))]
	p.dirty = true
	// 每条新提醒记一行去向，推送没收到时容器日志能答出这条推没推。
	for _, it := range fresh {
		switch {
		case it.T < *s.BaselineT:
			p.log.Infof("新提醒 %s：早于基线，跳过", describe(it))
		case !slices.Contains(cfg.Types, it.Type):
			p.log.Infof("新提醒 %s：类型不在推送范围，跳过", describe(it))
		default:
			payload, err := buildNotification(cfg.Source, it)
			if err != nil {
				p.log.Warnf("新提醒 %s：装不进推送，已跳过：%v", describe(it), err)
				continue
			}
			p.log.Infof("新提醒 %s：排队推送", describe(it))
			p.enqueue(payload)
		}
	}
}

// describe 在日志里标识一条提醒：id、类型、用户名、时间（UTC，和日志行首同一写法）。
// 正文和帖子标题不进日志。
func describe(it Item) string {
	id, _, _ := strings.Cut(it.ID, "|") // Feed 的 id 后面拼了时间和用户名，这里另外列出
	u := it.U
	if u == "" {
		u = "-"
	}
	return fmt.Sprintf("id=%s（%s，%s，%s）", id, it.Type, u, time.Unix(it.T, 0).UTC().Format("2006-01-02 15:04:05Z"))
}

// ---- 令牌过期检查（api 源）----

func (p *Poller) checkToken() {
	s, cfg := p.state, p.config
	r := p.fetchTokenInfo(cfg.PAT)
	now := p.now()
	p.dirty = true
	if r.err != "" {
		s.NextTokenCheck = now.Add(tokenCheckRetry).Unix()
		return
	}
	s.NextTokenCheck = now.Add(tokenCheckEvery).Unix()
	if r.invalid {
		return // 由轮询路径发 token_invalid
	}
	days := max(0, int(time.Unix(r.expiresAt, 0).Sub(now)/(24*time.Hour)))
	var crossed []int
	for _, d := range cfg.WarnDays {
		if days <= d && !slices.Contains(s.Warned, d) {
			crossed = append(crossed, d)
		}
	}
	if len(crossed) == 0 {
		return
	}
	s.Warned = append(s.Warned, crossed...)
	p.enqueue(marshal(systemPayload{Kind: "s", Code: "token_expiring", Days: &days, Exp: &r.expiresAt}))
	p.log.Infof("PAT 还有 %d 天过期，已发提醒", days)
}

// ---- 发往中继 ----

func (p *Poller) enqueue(plaintext []byte) {
	s := p.state
	s.Outbox = append(s.Outbox, p.box.Seal(PurposePush, plaintext))
	if over := len(s.Outbox) - outboxLimit; over > 0 {
		s.Outbox = s.Outbox[over:]
		p.log.Warnf("待发推送超过 %d 条，丢弃最早的 %d 条", outboxLimit, over)
	}
	p.nextFlush = time.Time{}
	p.dirty = true
}

func (p *Poller) flush() {
	s := p.state
	sent := 0
	var rerr *relayError
	for len(s.Outbox) > 0 {
		var rejected string
		if rejected, rerr = p.relay.push(s.Outbox[0]); rerr != nil {
			break
		}
		if rejected != "" {
			p.log.Warnf("中继拒收一条推送，已丢弃：%s", rejected)
		} else {
			sent++
		}
		s.Outbox = s.Outbox[1:]
		p.dirty = true
	}
	if sent > 0 {
		p.log.Infof("已交给中继 %d 条推送", sent)
	}
	switch {
	case rerr == nil:
	case rerr.kind == relayPayment:
		p.log.Warnf("订阅无效，中继暂停转发，丢弃 %d 条待发推送", len(s.Outbox))
		s.Outbox = nil
		p.dirty = true
	default:
		p.handleRelayError(rerr)
		wait := time.Minute
		if rerr.after > 0 {
			wait = rerr.after
		}
		p.nextFlush = p.now().Add(wait)
	}
}

// instanceMeta：机器信息 JSON（n 主机名、a 架构、t 启动时间），用加密密钥加密，中继看不到。
func instanceMeta(box *Box, hostname string, now time.Time) string {
	if r := []rune(hostname); len(r) > 64 {
		hostname = string(r[:64])
	}
	plain, _ := json.Marshal(struct {
		N string `json:"n"`
		A string `json:"a"`
		T int64  `json:"t"`
	}{hostname, runtime.GOARCH, now.Unix()})
	return box.Seal(PurposeInstance, plain)
}

// InstanceTag 是实例 id 的前 8 位，启动日志里打出来，方便和 App 里的列表对照。
func (p *Poller) InstanceTag() string { return p.inst[:8] }

func (p *Poller) heartbeat() {
	hb := heartbeatBody{Ver: p.version, State: p.status, Last: p.lastOK, Inst: p.inst, Meta: p.meta}
	if p.config != nil {
		v := p.config.V
		hb.Cfg = &v
	}
	latest, rerr := p.relay.heartbeat(hb)
	if rerr != nil && rerr.kind != relayPayment {
		p.handleRelayError(rerr)
		p.nextHeartbeat = p.now().Add(time.Minute)
		return
	}
	p.nextHeartbeat = p.now().Add(heartbeatEvery)
	if rerr == nil && latest > p.appliedVersion() {
		p.wantConfig = true
		p.nextConfigPull = time.Time{}
	}
}

// setStatus：状态变化时尽快发一次心跳，让 App 及时看到。
func (p *Poller) setStatus(next string) {
	if next == p.status {
		return
	}
	p.status = next
	p.nextHeartbeat = time.Time{}
}

// handleRelayError 记日志；推送 key 失效时暂停一切请求。订阅无效由调用方各自处理。
func (p *Poller) handleRelayError(e *relayError) {
	switch e.kind {
	case relayRevoked:
		p.revokedUntil = p.now().Add(revokedRetry)
		p.log.Warnf("推送 key 已失效：在 App 里重置过推送，请复制新的部署串重新部署")
	case relayRetry:
		p.log.Warnf("中继暂时不可用：%s", e.detail)
	}
}
