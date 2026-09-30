package notifier

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCryptoRoundTrip(t *testing.T) {
	box, _ := NewBox(testDeploy().EncKey)
	msg := `{"a":"中文"}`
	blob := box.Seal(PurposePush, []byte(msg))
	plain, err := box.Open(PurposePush, blob)
	if err != nil || string(plain) != msg {
		t.Fatalf("往返失败：%q %v", plain, err)
	}
	if _, err := box.Open(PurposeConfig, blob); err == nil {
		t.Fatal("用途不对时必须解密失败")
	}
	// 格式：nonce(12) ‖ 密文 ‖ tag(16)
	raw, _ := b64.DecodeString(blob)
	if len(raw) != 12+len(msg)+16 {
		t.Fatalf("长度 %d", len(raw))
	}
}

func TestDeployString(t *testing.T) {
	d := testDeploy()
	got, err := ParseDeployString("  " + buildDeployString(d) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.Relay != d.Relay || got.PushKey != d.PushKey || string(got.EncKey) != string(d.EncKey) {
		t.Fatalf("往返不一致：%+v", got)
	}
	d.Relay += "/"
	if got, _ := ParseDeployString(buildDeployString(d)); got.Relay != testRelay {
		t.Fatalf("结尾斜杠没去掉：%s", got.Relay)
	}
	for in, want := range map[string]string{"abc": "vvaepush1", "vvaepush1.@@": "无法解析"} {
		if _, err := ParseDeployString(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q：期望含 %q 的错误，得到 %v", in, want, err)
		}
	}
}

func TestDeployStrings(t *testing.T) {
	a := testDeploy()
	b := testDeploy()
	b.PushKey = b64.EncodeToString(make([]byte, 16))
	sa, sb := buildDeployString(a), buildDeployString(b)

	got, err := ParseDeployStrings(" " + sa + ",\n" + sb + " " + sa + ",")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PushKey != a.PushKey || got[1].PushKey != b.PushKey {
		t.Fatalf("应按顺序得到两个、重复的去掉：%+v", got)
	}
	if got[0].Tag() == got[1].Tag() || len(got[0].Tag()) != 8 {
		t.Fatalf("通道标识：%s %s", got[0].Tag(), got[1].Tag())
	}
	if _, err := ParseDeployStrings(sa + ",abc"); err == nil || !strings.Contains(err.Error(), "第 2 个") {
		t.Errorf("多个时要指出第几个坏了：%v", err)
	}
	if _, err := ParseDeployStrings(" ,\n"); err == nil {
		t.Error("空串应报错")
	}
}

func TestParseConfig(t *testing.T) {
	parse := func(s string) (*Config, error) { return ParseConfig([]byte(s)) }
	url := "https://www.v2ex.com/n/abc123.xml"
	if c, _ := parse(`{"v":1,"source":"feed","feedURL":"` + url + `","interval":5}`); c.Interval != 60 {
		t.Errorf("feed 间隔下限 60，得到 %d", c.Interval)
	}
	if _, err := parse(`{"v":1,"source":"feed","feedURL":"` + url + `?t=1"}`); err == nil || !strings.Contains(err.Error(), "地址不对") {
		t.Errorf("带查询参数的地址必须拒绝：%v", err)
	}
	if c, _ := parse(`{"v":2,"source":"api","pat":"12345678-aaaa","interval":1}`); c.Interval != 15 {
		t.Errorf("api 间隔下限 15，得到 %d", c.Interval)
	}
	if _, err := parse(`{"v":1,"source":"api"}`); err == nil || !strings.Contains(err.Error(), "PAT") {
		t.Errorf("缺 PAT 必须拒绝：%v", err)
	}
	if _, err := parse(`{"v":1.5,"source":"api","pat":"12345678"}`); err == nil {
		t.Error("版本号必须是整数")
	}
	c, _ := parse(`{"v":1,"source":"api","pat":"12345678","warnDays":[1,7,7,99],"types":["reply","bogus"]}`)
	if jsonString(c.WarnDays) != "[7,1]" {
		t.Errorf("warnDays 去重、去越界、从大到小：%v", c.WarnDays)
	}
	if len(c.Types) != 1 || c.Types[0] != TypeReply {
		t.Errorf("未知类型要丢掉：%v", c.Types)
	}
}

func jsonString(v any) string {
	j, _ := json.Marshal(v)
	return string(j)
}

func TestClassifyAndHTML(t *testing.T) {
	cases := map[string]ItemType{
		"alice 在回复 某帖 时提到了你":    TypeMention,
		"alice 在 某帖 里回复了你":      TypeReply,
		"bob 感谢了你在主题 › 某帖 里的回复": TypeThankReply,
		"bob 感谢了你发布的主题 › 某帖":    TypeThankTopic,
		"bob 收藏了你发布的主题 › 某帖":    TypeFavorite,
		"something else": TypeUnknown,
	}
	for in, want := range cases {
		if got := classify(in); got != want {
			t.Errorf("classify(%q) = %s，期望 %s", in, got, want)
		}
	}
	if got := htmlToText(`@<a href="/member/x">x</a> 你好<br />第二行 &amp; &#x4e2d;`); got != "@x 你好\n第二行 & 中" {
		t.Errorf("htmlToText：%q", got)
	}
}

func TestNormalizeNotification(t *testing.T) {
	norm := func(n testNotification) Item {
		var raw rawNotification
		json.Unmarshal([]byte(jsonString(n)), &raw)
		it, ok := normalizeNotification(raw)
		if !ok {
			t.Fatal("归一失败")
		}
		return it
	}
	it := norm(replyNotification(11, 1790000000, "alice", 123, 45))
	if it.Type != TypeReply || *it.Topic != 123 || *it.Floor != 45 || *it.TTL != "测试帖子 & 标题" || it.U != "alice" || *it.MID != 5011 {
		t.Errorf("回复归一不对：%+v", it)
	}
	thank := norm(thankNotification(12, 1790000000))
	if thank.Type != TypeThankReply || *thank.Topic != 200 || thank.Floor != nil {
		t.Errorf("感谢类的锚点是当时的回复数，不是楼层：%+v", thank)
	}
}

func TestParseFeed(t *testing.T) {
	items := parseFeed(feedXML(
		testEntry{"", "https://www.v2ex.com/t/300#reply41", "2026-09-28T14:47:06Z", "carol", "被感谢的回复"},
		testEntry{"dave 在回复 VVAE &amp; 推送 时提到了你", "https://www.v2ex.com/t/301#reply105", "2026-09-28T08:51:41Z", "dave",
			`@<a target="_blank" href="/member/me" rel="nofollow noopener">me</a> #89 内容`},
	))
	if len(items) != 2 {
		t.Fatalf("条数 %d", len(items))
	}
	if items[0].Type != TypeOther || items[0].Floor != nil || items[0].TTL != nil {
		t.Errorf("空标题归 other：%+v", items[0])
	}
	if items[0].ID != "tag:www.v2ex.com,2026-09-28:/t/300#reply41|2026-09-28T14:47:06Z|carol" {
		t.Errorf("去重键含发布时间与作者：%s", items[0].ID)
	}
	if it := items[1]; it.Type != TypeMention || *it.TTL != "VVAE & 推送" || *it.Floor != 105 || it.Body != "@me #89 内容" {
		t.Errorf("提到归一不对：%+v", it)
	}
}

func TestPayloadBudget(t *testing.T) {
	n := replyNotification(1, 1790000000, "alice", 100, 3)
	n.Payload = strings.Repeat("长", 3000) + strings.Repeat("😀", 100)
	var raw rawNotification
	json.Unmarshal([]byte(jsonString(n)), &raw)
	it, _ := normalizeNotification(raw)
	out, err := buildNotification(SourceAPI, it)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > PlaintextBudget {
		t.Fatalf("载荷 %d 字节，超出预算", len(out))
	}
	var obj struct {
		Cut  bool
		Body string
		Raw  string
	}
	json.Unmarshal(out, &obj)
	if !obj.Cut || !utf8.ValidString(obj.Body) || obj.Raw != it.Raw {
		t.Errorf("正文要截断且不切坏字符，raw 保持完整：cut=%v", obj.Cut)
	}
	// 加密并放进 APNs 载荷后不超过 4096 字节
	box, _ := NewBox(testDeploy().EncKey)
	apns := jsonString(map[string]any{
		"aps": map[string]any{"alert": map[string]string{"loc-key": "push.placeholder.body"}, "mutable-content": 1, "sound": "default"},
		"e":   box.Seal(PurposePush, out),
	})
	if len(apns) > 4096 {
		t.Fatalf("APNs 载荷 %d 字节", len(apns))
	}
}

func TestFileStore(t *testing.T) {
	dir := t.TempDir()
	fs := NewFileStore(dir, "state-x.json")
	if s, err := fs.Load(); err != nil || s.BaselineT != nil {
		t.Fatalf("没有文件时应返回空状态：%v", err)
	}
	bt := int64(123)
	if err := fs.Save(&State{BaselineT: &bt, Seen: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if s, _ := fs.Load(); *s.BaselineT != 123 || s.Seen[0] != "a" {
		t.Fatalf("往返不一致：%+v", s)
	}
	if st, _ := os.Stat(filepath.Join(dir, "state-x.json")); st.Mode().Perm() != 0o600 {
		t.Errorf("状态文件含配置密文，权限应为 600：%v", st.Mode())
	}
	os.WriteFile(filepath.Join(dir, "state-x.json"), []byte("{坏"), 0o600)
	if s, err := fs.Load(); err == nil || s == nil {
		t.Fatal("文件损坏时返回空状态和原因")
	}
}
