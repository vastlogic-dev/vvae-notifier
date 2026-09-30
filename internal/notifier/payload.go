package notifier

// 推送载荷：加密前的 JSON 不超过 2700 字节（APNs 单条上限 4096），超出先截 body，再截 raw。

import (
	"bytes"
	"encoding/json"
	"errors"
)

const PlaintextBudget = 2700

type notificationPayload struct {
	Kind  string   `json:"kind"`
	Src   Source   `json:"src"`
	ID    string   `json:"id"`
	T     int64    `json:"t"`
	Type  ItemType `json:"type"`
	U     string   `json:"u"`
	MID   *int64   `json:"mid,omitempty"`
	Topic *int64   `json:"topic"`
	Floor *int64   `json:"floor"`
	TTL   *string  `json:"ttl"`
	Body  string   `json:"body"`
	Cut   bool     `json:"cut"`
	Raw   string   `json:"raw"`
}

type systemPayload struct {
	Kind string `json:"kind"`
	Code string `json:"code"`
	Days *int   `json:"days,omitempty"`
	Exp  *int64 `json:"exp,omitempty"`
}

// marshal 不转义 < > &：raw 是 HTML，转义成 < 会白占预算。
func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v) // 只编码本文件的结构体，不会出错
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// fit 按码点截短 *field，让整个载荷不超过预算；返回是否截短过。
func fit(p *notificationPayload, field *string) bool {
	full := []rune(*field)
	fits := func(n int) bool {
		*field = string(full[:n])
		return len(marshal(p)) <= PlaintextBudget
	}
	if fits(len(full)) {
		return false
	}
	lo, hi := 0, len(full)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	*field = string(full[:lo])
	return true
}

func buildNotification(src Source, it Item) ([]byte, error) {
	p := &notificationPayload{
		Kind: "n", Src: src, ID: it.ID, T: it.T, Type: it.Type, U: it.U, MID: it.MID,
		Topic: it.Topic, Floor: it.Floor, TTL: it.TTL, Body: it.Body, Raw: it.Raw,
	}
	p.Cut = fit(p, &p.Body)
	fit(p, &p.Raw)
	out := marshal(p)
	if len(out) > PlaintextBudget {
		return nil, errors.New("载荷截断后仍超出预算")
	}
	return out, nil
}
