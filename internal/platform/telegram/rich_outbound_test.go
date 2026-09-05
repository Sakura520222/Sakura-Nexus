package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/botapi"
)

// ---------- fakes ----------

// fakeRichCall 记录一次 botapi 调用。
type fakeRichCall struct {
	method string
	params map[string]any
}

// fakeRich 按脚本回放错误（nil = 成功），脚本耗尽后重复最后一条。
type fakeRich struct {
	calls []fakeRichCall
	body  string // 预置 result 响应
	errs  []error
}

func (f *fakeRich) Call(_ context.Context, method string, params, result any) error {
	f.calls = append(f.calls, fakeRichCall{method: method, params: params.(map[string]any)})
	if n := len(f.errs); n > 0 {
		err := f.errs[len(f.errs)-1]
		if len(f.errs) > 1 {
			f.errs = f.errs[:n-1]
		}
		return err
	}
	if result != nil && f.body != "" {
		if err := json.Unmarshal([]byte(f.body), result); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeRich) methods() []string {
	ms := make([]string, len(f.calls))
	for i, c := range f.calls {
		ms[i] = c.method
	}
	return ms
}

// fakePlain 记录 MTProto 兜底调用。
type fakePlain struct {
	reqs []domain.SendRequest
}

func (f *fakePlain) send(_ context.Context, req domain.SendRequest) (domain.SentMessage, error) {
	f.reqs = append(f.reqs, req)
	return domain.SentMessage{Chat: req.Chat, MessageID: 77}, nil
}

// newRichTestOutbound 构造只走 Rich 路由的 Outbound（不触碰 gotd client）。
func newRichTestOutbound(fake richCaller, plain *fakePlain) *Outbound {
	o := &Outbound{}
	o.rich = newRichRouter(fake, plain.send, nil)
	return o
}

func contentReq(text string, style domain.SendStyle) domain.SendRequest {
	return domain.SendRequest{
		Chat:    domain.NewChatRef(domain.PeerChannel, 1234567890),
		Style:   style,
		Content: &domain.MessageContent{Text: text},
	}
}

// ---------- 参数构造 ----------

func TestBuildRichParams(t *testing.T) {
	req := domain.SendRequest{
		Chat:    domain.NewChatRef(domain.PeerChannel, 1234567890),
		ReplyTo: 321,
		Markup:  &domain.Keyboard{Rows: [][]domain.KeyboardButton{{{Text: "打开", URL: "https://e.io"}}}},
		Silent:  true,
	}
	params, err := buildRichParams("## 标题", req, true)
	if err != nil {
		t.Fatal(err)
	}
	if params["chat_id"] != int64(-1001234567890) {
		t.Errorf("chat_id 三态编码不符: %v", params["chat_id"])
	}
	rm, ok := params["rich_message"].(map[string]any)
	if !ok || rm["markdown"] != "## 标题" {
		t.Errorf("rich_message.markdown 不符: %v", params["rich_message"])
	}
	rp, ok := params["reply_parameters"].(map[string]any)
	if !ok || rp["message_id"] != int64(321) {
		t.Errorf("reply_parameters 不符: %v", params["reply_parameters"])
	}
	if _, bad := params["message_thread_id"]; bad {
		t.Error("ADR-008 硬限制：禁止 message_thread_id")
	}
	markup, ok := params["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("reply_markup 缺失: %v", params)
	}
	rows, ok := markup["inline_keyboard"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("inline_keyboard 行不符: %v", markup)
	}
	btn, ok := rows[0].([]any)[0].(map[string]any)
	if !ok || btn["text"] != "打开" || btn["url"] != "https://e.io" {
		t.Errorf("按钮映射不符: %v", rows[0])
	}
	if params["disable_notification"] != true {
		t.Errorf("disable_notification 不符: %v", params)
	}

	// 非首块：不带 reply_parameters（回复只标首块）。
	params2, err := buildRichParams("第二块", req, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := params2["reply_parameters"]; has {
		t.Error("续块不应携带 reply_parameters")
	}
}

// ---------- 路由 ----------

func TestSendRoutesContentToRich(t *testing.T) {
	fake := &fakeRich{body: `{"message_id":42}`}
	plain := &fakePlain{}
	o := newRichTestOutbound(fake, plain)

	msg, err := o.SendText(context.Background(), contentReq("# 标题\n\n正文", domain.StyleAuto))
	if err != nil {
		t.Fatal(err)
	}
	if msg.MessageID != 42 || msg.Chat != (domain.ChatRef{Kind: domain.PeerChannel, ID: 1234567890}) {
		t.Errorf("回执不符: %+v", msg)
	}
	if got := fake.methods(); len(got) != 1 || got[0] != "sendRichMessage" {
		t.Fatalf("应恰好一次 sendRichMessage: %v", got)
	}
	if len(plain.reqs) != 0 {
		t.Errorf("成功路径不应走 MTProto 兜底: %+v", plain.reqs)
	}
	rm := fake.calls[0].params["rich_message"].(map[string]any)
	if rm["markdown"] != "# 标题\n\n正文" {
		t.Errorf("markdown 应为规范化后内容: %v", rm)
	}
}

func TestSendStylePlainBypassesRich(t *testing.T) {
	fake := &fakeRich{body: `{"message_id":42}`}
	plain := &fakePlain{}
	o := newRichTestOutbound(fake, plain)

	// 未注入 peers：StylePlain 应在 MTProto 侧报错（证明未走 Rich 通道）。
	_, err := o.SendText(context.Background(), contentReq("# 标题", domain.StylePlain))
	if err == nil || !strings.Contains(err.Error(), "PeerResolver") {
		t.Fatalf("StylePlain 应走 MTProto（此处因无 peers 报错），得 %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("StylePlain 不应触碰 Rich 通道: %v", fake.methods())
	}
}

func TestSendRichMultiChunkReplyOnFirstOnly(t *testing.T) {
	fake := &fakeRich{body: `{"message_id":9}`}
	plain := &fakePlain{}
	o := newRichTestOutbound(fake, plain)

	// 三个 16000 字符段落 → 贪心切 2 条（16000×2+2=32002 一组，第三段落另起）。
	para := strings.Repeat("字", 16000)
	req := contentReq(para+"\n\n"+para+"\n\n"+para, domain.StyleAuto)
	req.ReplyTo = 5
	if _, err := o.SendText(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("应切 2 块两次调用，得 %d", len(fake.calls))
	}
	if _, has := fake.calls[0].params["reply_parameters"]; !has {
		t.Error("首块应带 reply_parameters")
	}
	if _, has := fake.calls[1].params["reply_parameters"]; has {
		t.Error("续块不应带 reply_parameters")
	}
}

// ---------- lazy capability 与 fallback 链（03 §2.7 / §2.9） ----------

func TestSendRich404KillsCapabilityAndFallsBack(t *testing.T) {
	// §2.9：首次真实发送 404 = method-not-supported → 置 flag 全部走 fallback；
	// 本条也降级，后续发送不再触碰 Rich。
	fake := &fakeRich{errs: []error{&botapi.APIError{Method: "sendRichMessage", Code: 404, Description: "Not Found"}}}
	plain := &fakePlain{}
	o := newRichTestOutbound(fake, plain)

	msg, err := o.SendText(context.Background(), contentReq("# 标题", domain.StyleAuto))
	if err != nil {
		t.Fatalf("404 应降级而非失败: %v", err)
	}
	if msg.MessageID != 77 {
		t.Errorf("降级后应返回 MTProto 回执: %+v", msg)
	}
	if len(plain.reqs) != 1 || plain.reqs[0].Text != "# 标题" {
		t.Errorf("降级应携带 Content.Text 走 MTProto: %+v", plain.reqs)
	}
	enabled, reason := o.RichCapability()
	if enabled || reason == "" {
		t.Errorf("capability 应禁用且带原因: enabled=%v reason=%q", enabled, reason)
	}

	// 第二次发送：Rich 不再被触碰。
	if _, err := o.SendText(context.Background(), contentReq("# 标题", domain.StyleAuto)); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 {
		t.Errorf("禁用后不应再调 Rich，累计 %d 次", len(fake.calls))
	}
	if len(plain.reqs) != 2 {
		t.Errorf("第二次应继续走 fallback: %d", len(plain.reqs))
	}
}

func TestSendRich400FormattingRejectFallsBackOnce(t *testing.T) {
	// §2.7：formatting reject → 本次降级；capability 保持（下次仍尝试 Rich）。
	fake := &fakeRich{errs: []error{&botapi.APIError{Method: "sendRichMessage", Code: 400, Description: "Bad Request: unsupported start tag"}}}
	plain := &fakePlain{}
	o := newRichTestOutbound(fake, plain)

	if _, err := o.SendText(context.Background(), contentReq("# 标题", domain.StyleAuto)); err != nil {
		t.Fatalf("400 应降级而非失败: %v", err)
	}
	if enabled, _ := o.RichCapability(); !enabled {
		t.Error("400 内容性拒绝不应禁用 capability")
	}
	// 下次仍尝试 Rich（fake 脚本重复末条 → 再次 400 → 再降级）。
	if _, err := o.SendText(context.Background(), contentReq("# 标题", domain.StyleAuto)); err != nil {
		t.Fatalf("第二次 400 仍应降级: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Errorf("400 后 capability 应保持，Rich 累计 %d 次调用", len(fake.calls))
	}
	if len(plain.reqs) != 2 {
		t.Errorf("两次都应降级: %d", len(plain.reqs))
	}
}

func TestSendRichTransientErrorsDoNotFallback(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"429 耗尽", &botapi.APIError{Method: "sendRichMessage", Code: 429, Description: "Too Many Requests"}},
		{"5xx 耗尽", &botapi.APIError{Method: "sendRichMessage", Code: 502, Description: "Bad Gateway"}},
		{"网络耗尽", errors.New("botapi: Post sendRichMessage: connection refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRich{errs: []error{tc.err}}
			plain := &fakePlain{}
			o := newRichTestOutbound(fake, plain)

			if _, err := o.SendText(context.Background(), contentReq("# 标题", domain.StyleAuto)); err == nil {
				t.Fatal("瞬态失败应返回错误（§1.4 failed + 可补发），不应降级")
			}
			if len(plain.reqs) != 0 {
				t.Errorf("瞬态失败不应降级重发: %+v", plain.reqs)
			}
			if enabled, _ := o.RichCapability(); !enabled {
				t.Error("瞬态失败不应禁用 capability")
			}
		})
	}
}

func TestSendRichUnsendableFallsBack(t *testing.T) {
	// ErrRichUnsendable（单行超 32768 无行边界）→ 本次降级，capability 保持。
	fake := &fakeRich{body: `{"message_id":1}`}
	plain := &fakePlain{}
	o := newRichTestOutbound(fake, plain)

	if _, err := o.SendText(context.Background(), contentReq(strings.Repeat("字", 40000), domain.StyleAuto)); err != nil {
		t.Fatalf("不可发送内容应降级: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("校验失败不应触网: %v", fake.methods())
	}
	if enabled, _ := o.RichCapability(); !enabled {
		t.Error("内容超限不应禁用 capability")
	}
	if len(plain.reqs) != 1 {
		t.Fatalf("应降级 MTProto: %d", len(plain.reqs))
	}
}

func TestSendStyleRichWhenUnavailableErrors(t *testing.T) {
	// StyleRich = 硬需求：Rich 未接线/已禁用时报错，不静默换道。
	plain := &fakePlain{}
	o := &Outbound{}
	o.rich = newRichRouter(nil, plain.send, nil) // 未接线

	if _, err := o.SendText(context.Background(), contentReq("# 标题", domain.StyleRich)); err == nil {
		t.Fatal("StyleRich 在通道不可用时应报错")
	}
	if len(plain.reqs) != 0 {
		t.Errorf("StyleRich 不可用不应静默走 MTProto: %+v", plain.reqs)
	}

	// Auto 在同样条件下走 fallback。
	if _, err := o.SendText(context.Background(), contentReq("# 标题", domain.StyleAuto)); err != nil {
		t.Fatalf("Auto 应走 fallback: %v", err)
	}
	if len(plain.reqs) != 1 {
		t.Errorf("Auto 应降级: %d", len(plain.reqs))
	}
}
