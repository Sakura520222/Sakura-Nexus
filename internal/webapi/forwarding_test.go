package webapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
	"github.com/Sakura520222/Sakura-Nexus/internal/forwarding"
)

// ---------- fakes ----------

type fakeRules struct {
	rules    map[int64]domain.ForwardRule
	nextID   int64
	created  int
	deleted  []int64
	setCalls [][2]any // {id, enabled}
	err      error
}

func newFakeRules() *fakeRules {
	return &fakeRules{rules: map[int64]domain.ForwardRule{}, nextID: 100}
}

func (f *fakeRules) List(context.Context) ([]domain.ForwardRule, error) {
	out := make([]domain.ForwardRule, 0, len(f.rules))
	for _, r := range f.rules {
		out = append(out, r)
	}
	return out, f.err
}

func (f *fakeRules) Get(_ context.Context, id int64) (domain.ForwardRule, bool, error) {
	r, ok := f.rules[id]
	return r, ok, f.err
}

func (f *fakeRules) Create(_ context.Context, rule domain.ForwardRule) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.nextID++
	rule.ID = f.nextID
	f.rules[rule.ID] = rule
	f.created++
	return rule.ID, nil
}

func (f *fakeRules) Update(_ context.Context, rule domain.ForwardRule) error {
	if _, ok := f.rules[rule.ID]; !ok {
		return errors.New("not found")
	}
	f.rules[rule.ID] = rule
	return f.err
}

func (f *fakeRules) Delete(_ context.Context, id int64) error {
	delete(f.rules, id)
	f.deleted = append(f.deleted, id)
	return f.err
}

func (f *fakeRules) SetEnabled(_ context.Context, id int64, enabled bool) error {
	r, ok := f.rules[id]
	if !ok {
		return errors.New("not found")
	}
	r.Enabled = enabled
	f.rules[id] = r
	f.setCalls = append(f.setCalls, [2]any{id, enabled})
	return f.err
}

type fakeChannels struct {
	chs     map[int64]domain.Channel
	deleted []int64
}

func newFakeChannels() *fakeChannels {
	return &fakeChannels{chs: map[int64]domain.Channel{}}
}

func (f *fakeChannels) List(context.Context) ([]domain.Channel, error) {
	out := make([]domain.Channel, 0, len(f.chs))
	for _, c := range f.chs {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeChannels) Upsert(_ context.Context, ch domain.Channel) error {
	f.chs[ch.TgID] = ch
	return nil
}

func (f *fakeChannels) Get(_ context.Context, tgID int64) (domain.Channel, bool, error) {
	c, ok := f.chs[tgID]
	return c, ok, nil
}

func (f *fakeChannels) Delete(_ context.Context, tgID int64) error {
	delete(f.chs, tgID)
	f.deleted = append(f.deleted, tgID)
	return nil
}

type fakeStats struct {
	rows []domain.ForwardingStat
}

func (f *fakeStats) Stats(_ context.Context, ruleID int64, _ int) ([]domain.ForwardingStat, error) {
	if ruleID == 0 {
		return f.rows, nil
	}
	var out []domain.ForwardingStat
	for _, r := range f.rows {
		if r.RuleID == ruleID {
			out = append(out, r)
		}
	}
	return out, nil
}

// newFCServer 构造注入 forwarding/channels 依赖的测试服务。
func newFCServer(t *testing.T) (*Server, *fakeEngine, *fakeRules, *fakeChannels, *fakeStats) {
	eng := &fakeEngine{}
	rules := newFakeRules()
	chs := newFakeChannels()
	stats := &fakeStats{rows: []domain.ForwardingStat{
		{RuleID: 1, Date: "2026-09-05", Forwarded: 3, Failed: 1},
	}}
	srv := NewServer("127.0.0.1", 0, nil,
		WithCredentials("admin", "pw-secret"),
		WithDeps(Deps{
			Engine:   eng,
			Rules:    rules,
			Channels: chs,
			Stats:    stats,
		}),
	)
	return srv, eng, rules, chs, stats
}

func TestRuleCRUDRoundTrip(t *testing.T) {
	srv, eng, rules, _, _ := newFCServer(t)
	ts, client := loginAndServe(t, srv)

	body := `{"name":"主频道","source":{"kind":"channel","id":"111"},"target":{"kind":"channel","id":"222"},` +
		`"enabled":true,"keywords":["推广"],"copyMode":"copy","delayMinSec":1,"delayMaxSec":2}`

	// create：字符串 ID 进、字符串 ID 出；触发规则热装载。
	resp, rbody := doJSON(t, client, "POST", ts.URL+"/api/forwarding/rules", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create 应 200: %d %s", resp.StatusCode, rbody)
	}
	if !strings.Contains(rbody, `"id":"101"`) {
		t.Errorf("create 应返回字符串 id: %s", rbody)
	}
	if eng.refreshes != 1 {
		t.Errorf("create 应触发 RefreshRules: %d", eng.refreshes)
	}

	// list：lastMessageId 字符串形态。
	resp, rbody = doJSON(t, client, "GET", ts.URL+"/api/forwarding/rules", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list 应 200: %d", resp.StatusCode)
	}
	if !strings.Contains(rbody, `"id":"101"`) || !strings.Contains(rbody, `"kind":"channel"`) {
		t.Errorf("list 形态不符: %s", rbody)
	}

	// update：lastMessageId 入参被忽略（引擎维护 cursor）。
	stored := rules.rules[101]
	stored.LastMessageID = 555
	rules.rules[101] = stored
	upd := `{"name":"改名","source":{"kind":"channel","id":"111"},"target":{"kind":"channel","id":"222"},"copyMode":"copy"}`
	resp, _ = doJSON(t, client, "PUT", ts.URL+"/api/forwarding/rules/101", upd)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update 应 200: %d", resp.StatusCode)
	}
	if rules.rules[101].Name != "改名" || rules.rules[101].LastMessageID != 555 {
		t.Errorf("update 应保留 cursor: %+v", rules.rules[101])
	}

	// enable/disable。
	resp, _ = doJSON(t, client, "POST", ts.URL+"/api/forwarding/rules/101/disable", "")
	if resp.StatusCode != http.StatusOK || rules.rules[101].Enabled {
		t.Errorf("disable 应生效: %d %+v", resp.StatusCode, rules.rules[101])
	}
	resp, _ = doJSON(t, client, "POST", ts.URL+"/api/forwarding/rules/101/enable", "")
	if resp.StatusCode != http.StatusOK || !rules.rules[101].Enabled {
		t.Errorf("enable 应生效: %d %+v", resp.StatusCode, rules.rules[101])
	}

	// delete。
	resp, _ = doJSON(t, client, "DELETE", ts.URL+"/api/forwarding/rules/101", "")
	if resp.StatusCode != http.StatusOK || len(rules.deleted) != 1 {
		t.Errorf("delete 应生效: %d", resp.StatusCode)
	}
}

func TestRuleValidationErrors(t *testing.T) {
	srv, _, rules, _, _ := newFCServer(t)
	ts, client := loginAndServe(t, srv)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"坏 kind", `{"source":{"kind":"bot","id":"1"},"target":{"kind":"channel","id":"2"}}`, http.StatusUnprocessableEntity},
		{"非数字 id", `{"source":{"kind":"channel","id":"abc"},"target":{"kind":"channel","id":"2"}}`, http.StatusUnprocessableEntity},
		{"坏 copyMode", `{"source":{"kind":"channel","id":"1"},"target":{"kind":"channel","id":"2"},"copyMode":"x"}`, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		resp, rbody := doJSON(t, client, "POST", ts.URL+"/api/forwarding/rules", tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: 应 %d 得 %d (%s)", tc.name, tc.want, resp.StatusCode, rbody)
		}
	}
	if rules.created != 0 {
		t.Errorf("校验失败不应触达仓库: %d", rules.created)
	}

	// 不存在的规则 → 404。
	resp, _ := doJSON(t, client, "GET", ts.URL+"/api/forwarding/rules/999", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("缺失规则应 404: %d", resp.StatusCode)
	}
}

func TestRuleBackfillEndpoint(t *testing.T) {
	srv, eng, _, _, _ := newFCServer(t)
	eng.backfillRes = forwarding.BackfillResult{Fetched: 10, Cursor: 555}
	ts, client := loginAndServe(t, srv)

	resp, rbody := doJSON(t, client, "POST", ts.URL+"/api/forwarding/rules/7/backfill", `{"limit":50}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backfill 应 200: %d %s", resp.StatusCode, rbody)
	}
	if len(eng.backfills) != 1 || eng.backfills[0][0] != 7 || eng.backfills[0][1] != 50 {
		t.Errorf("引擎调用参数不符: %+v", eng.backfills)
	}
	if !strings.Contains(rbody, `"fetched":10`) || !strings.Contains(rbody, `"cursor":"555"`) {
		t.Errorf("回执形态不符: %s", rbody)
	}

	// 引擎侧错误 → 422 用户可见。
	eng.backfillErr = errors.New("规则不存在或未启用: 7")
	resp, _ = doJSON(t, client, "POST", ts.URL+"/api/forwarding/rules/7/backfill", "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("引擎错误应 422: %d", resp.StatusCode)
	}
}

func TestStatsEndpoint(t *testing.T) {
	srv, _, _, _, stats := newFCServer(t)
	ts, client := loginAndServe(t, srv)

	resp, rbody := doJSON(t, client, "GET", ts.URL+"/api/forwarding/stats?rule_id=1&days=7", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats 应 200: %d", resp.StatusCode)
	}
	if !strings.Contains(rbody, `"ruleId":1`) || !strings.Contains(rbody, `"forwarded":3`) {
		t.Errorf("stats 形态不符: %s", rbody)
	}
	_ = stats
}

func TestChannelsCRUD(t *testing.T) {
	srv, _, _, chs, _ := newFCServer(t)
	ts, client := loginAndServe(t, srv)

	resp, _ := doJSON(t, client, "POST", ts.URL+"/api/channels",
		`{"tgId":"300","username":"srcfoo","title":"源频道","discussionChatId":"0"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("channel upsert 应 200: %d", resp.StatusCode)
	}
	if chs.chs[300].Username != "srcfoo" {
		t.Errorf("upsert 未落库: %+v", chs.chs[300])
	}

	resp, rbody := doJSON(t, client, "GET", ts.URL+"/api/channels/300", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(rbody, `"tgId":"300"`) {
		t.Errorf("channel get 不符: %d %s", resp.StatusCode, rbody)
	}

	resp, _ = doJSON(t, client, "DELETE", ts.URL+"/api/channels/300", "")
	if resp.StatusCode != http.StatusOK || len(chs.deleted) != 1 {
		t.Errorf("channel delete 应生效: %d", resp.StatusCode)
	}
}
