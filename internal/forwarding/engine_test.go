package forwarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// ---------- 测试替身 ----------

type fileSendCall struct {
	req   domain.SendRequest
	files []domain.LocalFile
}

type forwardCall struct {
	from domain.ChatRef
	ids  []int64
	to   domain.ChatRef
}

// fakeSender：记录三类发送调用；errs 按全局尝试序号注入错误（越界=nil）；
// gate 非 nil 时每次调用先阻塞等待（背压/关闭测试用）；sent 每次调用发信号。
type fakeSender struct {
	mu        sync.Mutex
	texts     []domain.SendRequest
	fileSends []fileSendCall
	forwards  []forwardCall
	attempts  int
	errs      []error
	gate      <-chan struct{}
	entered   chan struct{} // 非 nil：每次调用进入 begin 时先发信号（在 gate 等待之前）
	sent      chan struct{}
}

var _ Sender = (*fakeSender)(nil)

func (f *fakeSender) begin() error {
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	i := f.attempts
	f.attempts++
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	f.mu.Unlock()
	if f.sent != nil {
		select {
		case f.sent <- struct{}{}:
		default:
		}
	}
	return err
}

func (f *fakeSender) SendText(_ context.Context, req domain.SendRequest) (domain.SentMessage, error) {
	err := f.begin()
	f.mu.Lock()
	f.texts = append(f.texts, req)
	f.mu.Unlock()
	if err != nil {
		return domain.SentMessage{}, err
	}
	return domain.SentMessage{Chat: req.Chat, MessageID: 777000}, nil
}

func (f *fakeSender) SendFiles(_ context.Context, req domain.SendRequest, files []domain.LocalFile) (domain.SentMessage, error) {
	err := f.begin()
	f.mu.Lock()
	f.fileSends = append(f.fileSends, fileSendCall{req: req, files: append([]domain.LocalFile(nil), files...)})
	f.mu.Unlock()
	if err != nil {
		return domain.SentMessage{}, err
	}
	return domain.SentMessage{Chat: req.Chat, MessageID: 777001}, nil
}

func (f *fakeSender) ForwardMessages(_ context.Context, from domain.ChatRef, ids []int64, to domain.ChatRef) (domain.SentMessage, error) {
	err := f.begin()
	f.mu.Lock()
	f.forwards = append(f.forwards, forwardCall{from: from, ids: append([]int64(nil), ids...), to: to})
	f.mu.Unlock()
	if err != nil {
		return domain.SentMessage{}, err
	}
	return domain.SentMessage{Chat: to, MessageID: 777002}, nil
}

func (f *fakeSender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakeSender) textCalls() []domain.SendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.SendRequest(nil), f.texts...)
}

func (f *fakeSender) fileCalls() []fileSendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fileSendCall(nil), f.fileSends...)
}

func (f *fakeSender) forwardCalls() []forwardCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forwardCall(nil), f.forwards...)
}

type dedupKey struct {
	chat domain.ChatRef
	msg  int64
	tgt  domain.ChatRef
}

type recordCall struct {
	src     domain.MessageRef
	target  domain.ChatRef
	ruleID  int64
	sentMsg int64
	hash    string
}

type statCall struct {
	ruleID    int64
	forwarded bool
}

type fakeForwarded struct {
	mu       sync.Mutex
	exists   map[dedupKey]bool
	content  map[string]bool
	records  []recordCall
	stats    []statCall
	recorded chan struct{}
	statDone chan struct{}
}

var _ ForwardedStore = (*fakeForwarded)(nil)

func newFakeForwarded() *fakeForwarded {
	return &fakeForwarded{
		exists:   map[dedupKey]bool{},
		content:  map[string]bool{},
		recorded: make(chan struct{}, 64),
		statDone: make(chan struct{}, 64),
	}
}

func (f *fakeForwarded) Exists(_ context.Context, src domain.MessageRef, target domain.ChatRef) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exists[dedupKey{src.Chat, src.MessageID, target}], nil
}

func (f *fakeForwarded) ExistsByContent(_ context.Context, _, _ domain.ChatRef, hash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.content[hash], nil
}

func (f *fakeForwarded) Record(_ context.Context, src domain.MessageRef, target domain.ChatRef,
	ruleID int64, targetMessageID int64, contentHash string,
) error {
	f.mu.Lock()
	f.records = append(f.records, recordCall{src: src, target: target, ruleID: ruleID,
		sentMsg: targetMessageID, hash: contentHash})
	f.mu.Unlock()
	if f.recorded != nil {
		select {
		case f.recorded <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *fakeForwarded) IncrStats(_ context.Context, ruleID int64, forwarded bool) error {
	f.mu.Lock()
	f.stats = append(f.stats, statCall{ruleID: ruleID, forwarded: forwarded})
	f.mu.Unlock()
	if f.statDone != nil {
		select {
		case f.statDone <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *fakeForwarded) recordList() []recordCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordCall(nil), f.records...)
}

func (f *fakeForwarded) statList() []statCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]statCall(nil), f.stats...)
}

type advanceCall struct {
	id     int64
	cursor int64
}

type fakeRuleStore struct {
	mu       sync.Mutex
	rules    []domain.ForwardRule
	advances []advanceCall
	advanced chan struct{}
}

var _ RuleStore = (*fakeRuleStore)(nil)

func (f *fakeRuleStore) ListEnabled(_ context.Context) ([]domain.ForwardRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.ForwardRule(nil), f.rules...), nil
}

func (f *fakeRuleStore) AdvanceCursor(_ context.Context, id int64, cursor int64) error {
	f.mu.Lock()
	f.advances = append(f.advances, advanceCall{id: id, cursor: cursor})
	f.mu.Unlock()
	if f.advanced != nil {
		select {
		case f.advanced <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *fakeRuleStore) advanceList() []advanceCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]advanceCall(nil), f.advances...)
}

func (f *fakeRuleStore) setRules(rules []domain.ForwardRule) {
	f.mu.Lock()
	f.rules = rules
	f.mu.Unlock()
}

type fakeChannels map[int64]domain.Channel

var _ ChannelSource = fakeChannels(nil)

func (c fakeChannels) Get(_ context.Context, tgID int64) (domain.Channel, bool, error) {
	ch, ok := c[tgID]
	return ch, ok, nil
}

type fakeDownloader struct {
	mu        sync.Mutex
	calls     []string
	content   []byte
	freshSize int64  // 刷新后回传的 Size（模拟下载器返回新鲜元数据）
	freshName string // 刷新后回传的 FileName
}

var _ MediaDownloader = (*fakeDownloader)(nil)

func (f *fakeDownloader) DownloadMedia(_ context.Context, _ domain.ChannelMessage, media domain.MediaRef, dest string) (domain.MediaRef, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return media, err
	}
	if err := os.WriteFile(dest, f.content, 0o644); err != nil {
		return media, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, dest)
	f.mu.Unlock()
	if f.freshSize != 0 {
		media.Size = f.freshSize
	}
	if f.freshName != "" {
		media.FileName = f.freshName
	}
	return media, nil
}

func (f *fakeDownloader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ---------- 装配与同步助手 ----------

type sleepLog struct {
	mu   sync.Mutex
	durs []time.Duration
}

func (s *sleepLog) sleep(_ context.Context, d time.Duration) bool {
	s.mu.Lock()
	s.durs = append(s.durs, d)
	s.mu.Unlock()
	return true
}

func (s *sleepLog) list() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.durs...)
}

// zeroDelayParams：测试用参数（延迟 0、媒体上限极大、相册窗口 200ms/2000ms）。
func zeroDelayParams() ForwardingParams {
	return ForwardingParams{
		AlbumQuietMs:        200,
		AlbumHardDeadlineMs: 2000,
		MediaMaxSizeBytes:   1 << 40,
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestEngine 构造引擎并应用零延迟参数（默认 0.5–2s 延迟会让测试变慢）。
func newTestEngine(t *testing.T, rules []domain.ForwardRule, snd *fakeSender,
	dedup *fakeForwarded, dl *fakeDownloader, chans fakeChannels,
) (*Engine, *fakeRuleStore) {
	t.Helper()
	store := &fakeRuleStore{rules: rules, advanced: make(chan struct{}, 16)}
	e := NewEngine(EngineDeps{
		Rules:    store,
		Dedup:    dedup,
		Sender:   snd,
		Media:    dl,
		Channels: chans,
		TmpDir:   t.TempDir(), // 隔离临时目录（启动清理/任务目录不污染系统 /tmp）
		Log:      discardLogger(),
	})
	e.ApplySettings(zeroDelayParams())
	sl := &sleepLog{}
	e.sleep = sl.sleep
	t.Cleanup(func() { _ = sl })
	return e, store
}

// startEngine 先同步装载规则（消除 Run 异步 RefreshRules 与首条消息的竞态），
// 再启动 Run 循环；返回取消函数与 Run 结果等待（幂等，测试体与 Cleanup 可各调一次）。
func startEngine(t *testing.T, e *Engine) (cancel context.CancelFunc, waitDone func() error) {
	t.Helper()
	if err := e.RefreshRules(context.Background()); err != nil {
		t.Fatalf("预装载规则: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	var (
		once   sync.Once
		runErr error
	)
	waitDone = func() error {
		once.Do(func() {
			select {
			case runErr = <-done:
			case <-time.After(2 * time.Second):
				runErr = errors.New("Run 未在 2s 内退出")
			}
		})
		return runErr
	}
	t.Cleanup(func() {
		cancel()
		if err := waitDone(); err != nil {
			t.Errorf("Run 收尾: %v", err)
		}
	})
	return cancel, waitDone
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待 %s 超时", what)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// cursorOf 读取规则当前内存 cursor（含被 unresolved 阻挡的真实位置）。
func cursorOf(e *Engine, ruleID int64) int64 {
	e.trackMu.Lock()
	defer e.trackMu.Unlock()
	if tr, ok := e.trackers[ruleID]; ok {
		return tr.current()
	}
	return -1
}

// pendingOf 读取规则未终结消息数。
func pendingOf(e *Engine, ruleID int64) int {
	e.trackMu.Lock()
	defer e.trackMu.Unlock()
	if tr, ok := e.trackers[ruleID]; ok {
		return tr.pending()
	}
	return -1
}

// ---------- 消息与规则构造 ----------

func textMsg(chatID, msgID int64, text string) domain.ChannelMessage {
	return domain.ChannelMessage{
		Ref:         domain.MessageRef{Chat: domain.NewChatRef(domain.PeerChannel, chatID), MessageID: msgID},
		SourceType:  "channel_message",
		Text:        text,
		PublishedAt: time.Unix(1700000000, 0),
	}
}

func mediaMsg(chatID, msgID int64, caption, mediaType string) domain.ChannelMessage {
	m := textMsg(chatID, msgID, caption)
	m.Media = []domain.MediaRef{{Key: "photo:0", Type: mediaType, MimeType: "image/jpeg", Size: 10}}
	return m
}

func albumMember(chatID, msgID, groupID int64, caption, mediaType string) domain.ChannelMessage {
	m := mediaMsg(chatID, msgID, caption, mediaType)
	m.GroupedID = groupID
	return m
}

func rule(id, srcID, dstID int64) domain.ForwardRule {
	return domain.ForwardRule{
		ID: id, Enabled: true, CopyMode: "copy",
		Source: domain.NewChatRef(domain.PeerChannel, srcID),
		Target: domain.NewChatRef(domain.PeerChannel, dstID),
	}
}

// ---------- 基础转发链 ----------

func TestEngineTextForwardHappyPath(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	e, store := newTestEngine(t, []domain.ForwardRule{rule(1, 100, 200)}, snd, dedup, nil, nil)
	startEngine(t, e)

	src := textMsg(100, 42, "hello *world*")
	src.Entities = []domain.Entity{{Type: "bold", Offset: 6, Length: 5}}
	e.HandleNew(context.Background(), src)

	waitSignal(t, store.advanced, "cursor 推进")

	calls := snd.textCalls()
	if len(calls) != 1 {
		t.Fatalf("应恰好一次 SendText，实际 %d", len(calls))
	}
	if calls[0].Chat.ID != 200 || calls[0].Chat.Kind != domain.PeerChannel {
		t.Errorf("发送目标不符: %+v", calls[0].Chat)
	}
	if calls[0].Text != "hello *world*" || len(calls[0].Entities) != 1 {
		t.Errorf("文本/entities 应透传: text=%q entities=%d", calls[0].Text, len(calls[0].Entities))
	}
	recs := dedup.recordList()
	if len(recs) != 1 {
		t.Fatalf("应记录一条去重，实际 %d", len(recs))
	}
	if recs[0].src.MessageID != 42 || recs[0].src.Chat.ID != 100 ||
		recs[0].target.ID != 200 || recs[0].ruleID != 1 || recs[0].sentMsg != 777000 {
		t.Errorf("去重记录五列键不符: %+v", recs[0])
	}
	wantHash := sha256Hex("hello *world*")
	if recs[0].hash != wantHash {
		t.Errorf("content_hash 应为聚合文本 sha256: got %s want %s", recs[0].hash, wantHash)
	}
	stats := dedup.statList()
	if len(stats) != 1 || !stats[0].forwarded || stats[0].ruleID != 1 {
		t.Errorf("应计一次真实成功: %+v", stats)
	}
	adv := store.advanceList()
	if len(adv) != 1 || adv[0].cursor != 42 {
		t.Errorf("cursor 应推进到 42: %+v", adv)
	}
}

// 多规则命中 → 逐规则独立处理；单规则失败不影响其他（03 §3.1）。
func TestEngineMultipleRulesIndependent(t *testing.T) {
	snd, dedup := &fakeSender{errs: []error{
		nil,                                                        // 规则 A 成功
		errors.New("boom"), errors.New("boom"), errors.New("boom"), // 规则 B 三次尝试均失败
	}}, newFakeForwarded()
	ruleA, ruleB := rule(1, 100, 200), rule(2, 100, 300)
	e, store := newTestEngine(t, []domain.ForwardRule{ruleA, ruleB}, snd, dedup, nil, nil)
	startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(100, 7, "hi"))

	waitSignal(t, dedup.statDone, "规则 A 统计")
	eventually(t, "规则 B 统计（失败）", func() bool { return len(dedup.statList()) == 2 })

	stats := dedup.statList()
	if stats[0].ruleID != 1 || !stats[0].forwarded {
		t.Errorf("规则 A 应成功: %+v", stats[0])
	}
	if stats[1].ruleID != 2 || stats[1].forwarded {
		t.Errorf("规则 B 应失败计数: %+v", stats[1])
	}
	if snd.callCount() != 4 { // A×1 + B×3 次尝试
		t.Errorf("尝试次数应为 4，实际 %d", snd.callCount())
	}
	if got := cursorOf(e, 1); got != 7 {
		t.Errorf("规则 A cursor 应为 7，实际 %d", got)
	}
	if got := cursorOf(e, 2); got != 0 {
		t.Errorf("规则 B cursor 不应推进（transient 失败），实际 %d", got)
	}
	adv := store.advanceList()
	if len(adv) != 1 || adv[0].id != 1 || adv[0].cursor != 7 {
		t.Errorf("仅规则 A 持久化 cursor: %+v", adv)
	}
	recs := dedup.recordList()
	if len(recs) != 1 || recs[0].ruleID != 1 {
		t.Errorf("仅规则 A 记录去重: %+v", recs)
	}
}

// 过滤拒绝 = terminal：不发送、不计数，但 cursor 推进（P0 Plan §6）。
func TestEngineFilteredIsTerminalWithoutStats(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	r := rule(1, 100, 200)
	r.Keywords = []string{"sakura"}
	e, store := newTestEngine(t, []domain.ForwardRule{r}, snd, dedup, nil, nil)
	startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(100, 55, "unrelated"))
	waitSignal(t, store.advanced, "filtered 终结推进")

	if snd.callCount() != 0 {
		t.Error("被过滤消息不应发送")
	}
	if got := dedup.recordList(); len(got) != 0 {
		t.Errorf("被过滤消息不应记录: %+v", got)
	}
	if got := dedup.statList(); len(got) != 0 {
		t.Errorf("被过滤消息不应计数: %+v", got)
	}
	if adv := store.advanceList(); len(adv) != 1 || adv[0].cursor != 55 {
		t.Errorf("filtered 应推进 cursor: %+v", adv)
	}
}

// 五列键已存在（dedup already-sent）= terminal：跳过、推进 cursor、不重复计数。
func TestEngineDedupSkipIsTerminal(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	r := rule(1, 100, 200)
	e, store := newTestEngine(t, []domain.ForwardRule{r}, snd, dedup, nil, nil)
	startEngine(t, e)
	dedup.mu.Lock()
	dedup.exists[dedupKey{domain.NewChatRef(domain.PeerChannel, 100), 66, r.Target}] = true
	dedup.mu.Unlock()

	e.HandleNew(context.Background(), textMsg(100, 66, "dup"))
	waitSignal(t, store.advanced, "dedup 跳过推进")

	if snd.callCount() != 0 {
		t.Error("已转发消息不应重发")
	}
	if len(dedup.recordList()) != 0 || len(dedup.statList()) != 0 {
		t.Error("dedup 跳过不应写记录/计数")
	}
	if adv := store.advanceList(); len(adv) != 1 || adv[0].cursor != 66 {
		t.Errorf("dedup already-sent 应推进 cursor: %+v", adv)
	}
}

// P0 Plan §6 冻结场景：100 失败（transient 耗尽）；101、102 成功 → cursor 保持
// 初始 99 不越过；100 恢复成功后连续推进至 102。
func TestEngineCursorNeverCrossesTransientFailure(t *testing.T) {
	snd, dedup := &fakeSender{errs: []error{
		errors.New("flood"), errors.New("flood"), errors.New("flood"), // 100 的三次尝试
	}}, newFakeForwarded()
	r := rule(1, 100, 200)
	r.LastMessageID = 99
	e, store := newTestEngine(t, []domain.ForwardRule{r}, snd, dedup, nil, nil)
	startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(100, 100, "m100"))
	e.HandleNew(context.Background(), textMsg(100, 101, "m101"))
	e.HandleNew(context.Background(), textMsg(100, 102, "m102"))

	eventually(t, "三条消息处理完成（3 次统计）", func() bool { return len(dedup.statList()) == 3 })

	if got := cursorOf(e, 1); got != 99 {
		t.Fatalf("cursor 必须保持 99（transient 失败不得越过），实际 %d", got)
	}
	if len(store.advanceList()) != 0 {
		t.Fatalf("不应有任何持久化推进: %+v", store.advanceList())
	}
	recs := dedup.recordList()
	if len(recs) != 2 { // 101、102 成功
		t.Fatalf("应恰好两条成功记录，实际 %d", len(recs))
	}

	// 恢复：100 重投（backfill 语义）→ 成功 → 连续推进至 102
	e.HandleNew(context.Background(), textMsg(100, 100, "m100"))
	waitSignal(t, store.advanced, "恢复后推进")

	if got := cursorOf(e, 1); got != 102 {
		t.Errorf("恢复后 cursor 应连续推进至 102，实际 %d", got)
	}
	adv := store.advanceList()
	if len(adv) != 1 || adv[0].cursor != 102 {
		t.Errorf("持久化推进应为 102: %+v", adv)
	}
	if pendingOf(e, 1) != 0 {
		t.Errorf("不应残留 unresolved: %d", pendingOf(e, 1))
	}
}

// 永久性失败（分类器判定）：单次尝试即标记 terminal，cursor 推进避免卡死（§6）。
func TestEnginePermanentFailureMarksTerminal(t *testing.T) {
	permErr := errors.New("CHAT_ADMIN_REQUIRED")
	snd, dedup := &fakeSender{errs: []error{permErr, permErr, permErr, permErr}}, newFakeForwarded()
	r := rule(1, 100, 200)
	e, store := newTestEngine(t, []domain.ForwardRule{r}, snd, dedup, nil, nil)
	e.deps.Classify = func(err error) SendFailureKind {
		if errors.Is(err, permErr) {
			return FailurePermanent
		}
		return FailureTransient
	}
	startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(100, 30, "gone"))
	waitSignal(t, store.advanced, "永久失败终结推进")

	if snd.callCount() != 1 {
		t.Errorf("永久失败不应重试（尝试 %d 次）", snd.callCount())
	}
	stats := dedup.statList()
	if len(stats) != 1 || stats[0].forwarded {
		t.Errorf("应计一次失败: %+v", stats)
	}
	if got := cursorOf(e, 1); got != 30 {
		t.Errorf("永久失败应标记 terminal 推进 cursor，实际 %d", got)
	}
}

// content_dedup 开启：同源同目标同内容命中 → 跳过且推进（防删帖重发，03 §3.5）。
func TestEngineContentDedupSkips(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	r := rule(1, 100, 200)
	e, store := newTestEngine(t, []domain.ForwardRule{r}, snd, dedup, nil, nil)
	p := zeroDelayParams()
	p.ContentDedup = true
	e.ApplySettings(p)
	dedup.mu.Lock()
	dedup.content[sha256Hex("reposted")] = true
	dedup.mu.Unlock()
	startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(100, 88, "reposted"))
	waitSignal(t, store.advanced, "内容去重推进")

	if snd.callCount() != 0 {
		t.Error("内容重复不应发送")
	}
	if len(dedup.recordList()) != 0 || len(dedup.statList()) != 0 {
		t.Error("内容去重跳过不应记录/计数")
	}
	if adv := store.advanceList(); len(adv) != 1 || adv[0].cursor != 88 {
		t.Errorf("内容去重应推进 cursor: %+v", adv)
	}
}

// content_dedup 关闭时同内容不拦截（配置语义验证）。
func TestEngineContentDedupDisabledAllows(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	r := rule(1, 100, 200)
	e, store := newTestEngine(t, []domain.ForwardRule{r}, snd, dedup, nil, nil)
	dedup.mu.Lock()
	dedup.content[sha256Hex("reposted")] = true
	dedup.mu.Unlock()
	startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(100, 88, "reposted"))
	waitSignal(t, store.advanced, "正常转发推进")

	if snd.callCount() != 1 {
		t.Error("content_dedup 关闭时同内容应正常发送")
	}
}

// ---------- 相册 ----------

func TestEngineAlbumAggregatedSendWithAllMemberDedup(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	e, store := newTestEngine(t, []domain.ForwardRule{rule(1, 100, 200)}, snd, dedup,
		&fakeDownloader{content: []byte("img")}, nil)
	startEngine(t, e)

	// 注入假时钟驱动相册窗口（albumLoop 的真实 ticker 不参与本测试）
	fc := &fakeClock{mu: time.Unix(1700000000, 0)}
	e.albumMu.Lock()
	e.album.clock = fc
	e.albumMu.Unlock()

	e.HandleNew(context.Background(), albumMember(100, 10, 7, "caption A", "photo"))
	e.HandleNew(context.Background(), albumMember(100, 11, 7, "caption B", "photo"))
	e.HandleNew(context.Background(), albumMember(100, 12, 7, "", "video"))
	if snd.callCount() != 0 {
		t.Fatal("窗口未满不应发送")
	}

	fc.advance(250 * time.Millisecond) // 越过 quiet 窗口
	e.drainAlbum(context.Background())
	waitSignal(t, store.advanced, "相册整组推进")

	calls := snd.fileCalls()
	if len(calls) != 1 {
		t.Fatalf("相册应整体一次 SendFiles，实际 %d", len(calls))
	}
	if len(calls[0].files) != 3 {
		t.Errorf("应携带 3 个成员文件，实际 %d", len(calls[0].files))
	}
	if calls[0].req.Caption != "caption A\ncaption B" {
		t.Errorf("caption 应为聚合文本: %q", calls[0].req.Caption)
	}
	if calls[0].req.Chat.ID != 200 {
		t.Errorf("目标不符: %+v", calls[0].req.Chat)
	}
	recs := dedup.recordList()
	if len(recs) != 3 {
		t.Fatalf("相册全成员去重应记录 3 条，实际 %d", len(recs))
	}
	for _, rc := range recs {
		if rc.target.ID != 200 || rc.sentMsg != 777001 {
			t.Errorf("成员记录目标/回执不符: %+v", rc)
		}
	}
	if stats := dedup.statList(); len(stats) != 1 || !stats[0].forwarded {
		t.Errorf("相册按一次发送计一次成功: %+v", stats)
	}
	if adv := store.advanceList(); len(adv) != 1 || adv[0].cursor != 12 {
		t.Errorf("相册 cursor 应推进至最大成员 12: %+v", adv)
	}
}

// 窗口后迟到成员：聚合器按独立新组处理（03 §1.6）→ 第二次独立发送。
func TestEngineLateAlbumMemberBecomesNewGroup(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	e, store := newTestEngine(t, []domain.ForwardRule{rule(1, 100, 200)}, snd, dedup,
		&fakeDownloader{content: []byte("img")}, nil)
	startEngine(t, e)
	fc := &fakeClock{mu: time.Unix(1700000000, 0)}
	e.albumMu.Lock()
	e.album.clock = fc
	e.albumMu.Unlock()

	e.HandleNew(context.Background(), albumMember(100, 20, 9, "first", "photo"))
	fc.advance(250 * time.Millisecond)
	e.drainAlbum(context.Background())
	waitSignal(t, store.advanced, "第一组推进")

	// 迟到成员：同 grouped_id，但窗口已过
	e.HandleNew(context.Background(), albumMember(100, 21, 9, "late", "photo"))
	fc.advance(250 * time.Millisecond)
	e.drainAlbum(context.Background())
	eventually(t, "迟到成员独立成组发送", func() bool { return len(snd.fileCalls()) == 2 })

	calls := snd.fileCalls()
	if len(calls[1].files) != 1 {
		t.Errorf("迟到成员应为单文件独立组: %+v", calls[1].files)
	}
	if calls[1].req.Caption != "late" {
		t.Errorf("迟到成员 caption: %q", calls[1].req.Caption)
	}
	recs := dedup.recordList()
	if len(recs) != 2 {
		t.Errorf("两组成员各自记录，实际 %d", len(recs))
	}
}

// ---------- 媒体与 forward 模式 ----------

func TestEngineMediaMessageDownloadsAndCleansUp(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	dl := &fakeDownloader{content: []byte("jpeg-bytes")}
	e, store := newTestEngine(t, []domain.ForwardRule{rule(1, 100, 200)}, snd, dedup, dl, nil)
	startEngine(t, e)

	e.HandleNew(context.Background(), mediaMsg(100, 77, "pic caption", "photo"))
	waitSignal(t, store.advanced, "媒体转发推进")

	calls := snd.fileCalls()
	if len(calls) != 1 || len(calls[0].files) != 1 {
		t.Fatalf("应一次单文件发送: %+v", calls)
	}
	f := calls[0].files[0]
	if f.Meta.Type != "photo" {
		t.Errorf("文件元数据应透传: %+v", f.Meta)
	}
	if _, err := os.Stat(f.Path); err == nil {
		t.Errorf("临时文件应已删除: %s", f.Path)
	}
	if calls[0].req.Caption != "pic caption" {
		t.Errorf("caption 应为消息文本: %q", calls[0].req.Caption)
	}
	if dl.callCount() != 1 {
		t.Errorf("应恰好一次下载，实际 %d", dl.callCount())
	}
}

// copy_mode=forward：原样转发（ForwardMessages），不做下载/重打包（03 §3.3 ③）。
func TestEngineForwardModeUsesForwardMessages(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	dl := &fakeDownloader{}
	r := rule(1, 100, 200)
	r.CopyMode = "forward"
	e, store := newTestEngine(t, []domain.ForwardRule{r}, snd, dedup, dl, nil)
	startEngine(t, e)

	e.HandleNew(context.Background(), mediaMsg(100, 90, "any", "photo"))
	waitSignal(t, store.advanced, "forward 推进")

	fwd := snd.forwardCalls()
	if len(fwd) != 1 {
		t.Fatalf("应一次 ForwardMessages，实际 %d", len(fwd))
	}
	if fwd[0].from.ID != 100 || fwd[0].to.ID != 200 || len(fwd[0].ids) != 1 || fwd[0].ids[0] != 90 {
		t.Errorf("转发参数不符: %+v", fwd[0])
	}
	if dl.callCount() != 0 {
		t.Error("forward 模式不应下载媒体")
	}
	if snd.callCount() != 1 || len(snd.textCalls()) != 0 || len(snd.fileCalls()) != 0 {
		t.Error("forward 模式不应走文本/文件通道")
	}
	recs := dedup.recordList()
	if len(recs) != 1 || recs[0].sentMsg != 777002 {
		t.Errorf("forward 成功应记录: %+v", recs)
	}
}

// ---------- 队列与延迟 ----------

// 容量 100 阻塞背压：队列满时 HandleNew 阻塞；消费者放行后全部完成（01 §5.2）。
func TestEngineQueueBackpressureBlocksWhenFull(t *testing.T) {
	gate := make(chan struct{})
	snd := &fakeSender{gate: gate}
	dedup := newFakeForwarded()
	e, _ := newTestEngine(t, []domain.ForwardRule{rule(1, 100, 200)}, snd, dedup, nil, nil)
	startEngine(t, e)

	if cap(e.queue) != 100 {
		t.Fatalf("队列容量应为 100，实际 %d", cap(e.queue))
	}

	// 消费者阻塞在第 1 条 → 队列回填 100 条 → 第 102 条 HandleNew 应阻塞
	for i := 1; i <= 101; i++ {
		e.HandleNew(context.Background(), textMsg(100, int64(i), "m"))
	}
	blocked := make(chan struct{})
	go func() {
		e.HandleNew(context.Background(), textMsg(100, 102, "m"))
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("队列满时 HandleNew 不应立即返回（应阻塞背压）")
	case <-time.After(200 * time.Millisecond):
	}

	close(gate) // 放行全部
	eventually(t, "102 次发送完成", func() bool { return snd.callCount() == 102 })
	eventually(t, "102 条记录完成", func() bool { return len(dedup.recordList()) == 102 })
	eventually(t, "全部入队", func() bool { return e.QueueLen() == 0 })
}

func TestEngineRandomDelayBounds(t *testing.T) {
	e, _ := newTestEngine(t, nil, &fakeSender{}, newFakeForwarded(), nil, nil)
	p := zeroDelayParams()

	cases := []struct {
		name        string
		ruleDelay   [2]float64
		paramsDelay [2]float64
		rnd         float64
		want        time.Duration
	}{
		{"规则区间中点", [2]float64{1, 2}, [2]float64{0.3, 0.9}, 0.5, 1500 * time.Millisecond},
		{"规则下界", [2]float64{1, 2}, [2]float64{0.3, 0.9}, 0, 1000 * time.Millisecond},
		{"规则未配置回落默认", [2]float64{0, 0}, [2]float64{0.3, 0.9}, 0.5, 600 * time.Millisecond},
		{"规则固定值", [2]float64{5, 5}, [2]float64{0.3, 0.9}, 0.5, 5 * time.Second},
		{"非法区间回落默认", [2]float64{3, 1}, [2]float64{0.3, 0.9}, 0.5, 600 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pp := p
			pp.DefaultDelayMinSec, pp.DefaultDelayMaxSec = tc.paramsDelay[0], tc.paramsDelay[1]
			e.ApplySettings(pp)
			e.rand = func() float64 { return tc.rnd }
			r := rule(1, 100, 200)
			r.DelayMinSec, r.DelayMaxSec = tc.ruleDelay[0], tc.ruleDelay[1]
			if got := e.delayFor(r); got != tc.want {
				t.Errorf("delayFor = %v, want %v", got, tc.want)
			}
		})
	}
}

// 消费者发送前入睡随机延迟；热更参数对后续任务生效。
func TestEngineDelayAppliedBeforeSendAndSettingsHotReload(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	e, _ := newTestEngine(t, []domain.ForwardRule{rule(1, 100, 200)}, snd, dedup, nil, nil)
	sl := &sleepLog{}
	e.sleep = sl.sleep
	startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(100, 1, "first"))
	waitSignal(t, dedup.recorded, "首条完成")

	p := zeroDelayParams()
	p.DefaultDelayMinSec, p.DefaultDelayMaxSec = 7, 7
	e.ApplySettings(p)

	e.HandleNew(context.Background(), textMsg(100, 2, "second"))
	waitSignal(t, dedup.recorded, "次条完成")

	durs := sl.list()
	if len(durs) != 2 {
		t.Fatalf("应记录两次 sleep，实际 %d", len(durs))
	}
	if durs[1] != 7*time.Second {
		t.Errorf("热更后延迟应为 7s，实际 %v", durs[1])
	}
}

// ---------- 规则匹配边界 ----------

func TestEngineUsernameRuleMatch(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	r := rule(1, 999, 200) // 源 ID 故意不匹配
	r.SourceUsername = "@Foo"
	chans := fakeChannels{555: {TgID: 555, Username: "foo"}}
	e, store := newTestEngine(t, []domain.ForwardRule{r}, snd, dedup, nil, chans)
	startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(555, 3, "via username"))
	waitSignal(t, store.advanced, "username 命中推进")

	if snd.callCount() != 1 {
		t.Errorf("username 辅助列命中应发送，实际 %d 次", snd.callCount())
	}
}

func TestEngineUsernameMatchWithoutChannelSourceSkips(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	r := rule(1, 999, 200)
	r.SourceUsername = "@Foo"
	e, _ := newTestEngine(t, []domain.ForwardRule{r}, snd, dedup, nil, nil) // 无 ChannelSource
	startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(555, 3, "no resolver"))
	eventually(t, "无匹配路径稳定", func() bool { return e.QueueLen() == 0 })

	if snd.callCount() != 0 {
		t.Error("无 ChannelSource 时 username 规则不应命中")
	}
}

func TestEngineNoRuleMatchNoSideEffects(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	e, store := newTestEngine(t, []domain.ForwardRule{rule(1, 100, 200)}, snd, dedup, nil, nil)
	startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(777, 1, "nobody"))
	eventually(t, "无匹配路径稳定", func() bool { return e.QueueLen() == 0 })

	if snd.callCount() != 0 || len(dedup.recordList()) != 0 || len(store.advanceList()) != 0 {
		t.Error("无规则命中的消息不应产生任何副作用")
	}
}

func TestEngineRefreshRulesPicksUpNewRule(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	e, store := newTestEngine(t, nil, snd, dedup, nil, nil)
	startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(100, 1, "before"))
	if e.QueueLen() != 0 || snd.callCount() != 0 {
		t.Fatal("无规则时不应入队")
	}

	store.setRules([]domain.ForwardRule{rule(5, 100, 200)})
	if err := e.RefreshRules(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.HandleNew(context.Background(), textMsg(100, 2, "after"))
	waitSignal(t, dedup.recorded, "新规则生效")

	recs := dedup.recordList()
	if len(recs) != 1 || recs[0].ruleID != 5 {
		t.Errorf("刷新后新规则应生效: %+v", recs)
	}
}

// ---------- 生命周期 ----------

// 关闭语义：ctx 取消 → 消费者完成当前任务后退出；队列剩余任务丢弃且不计数
// （消息未转发、cursor 未越过 → 回溯补发恢复，P0 Plan §6）。
func TestEngineShutdownFinishesCurrentDropsQueued(t *testing.T) {
	gate := make(chan struct{})
	snd := &fakeSender{gate: gate, entered: make(chan struct{}, 1)}
	dedup := newFakeForwarded()
	e, _ := newTestEngine(t, []domain.ForwardRule{rule(1, 100, 200)}, snd, dedup, nil, nil)
	cancel, waitDone := startEngine(t, e)

	e.HandleNew(context.Background(), textMsg(100, 1, "in-flight"))
	waitSignal(t, snd.entered, "消费者进入第一条发送") // 消费者此刻阻塞在 gate
	e.HandleNew(context.Background(), textMsg(100, 2, "queued"))
	if got := e.QueueLen(); got != 1 {
		t.Fatalf("第二条应在队列中，QueueLen=%d", got)
	}

	cancel()    // 请求关闭（消费者仍阻塞在第一条发送）
	close(gate) // 放行第一条
	if err := waitDone(); err != nil {
		t.Fatalf("Run 应正常退出，err=%v", err)
	}

	if recs := dedup.recordList(); len(recs) != 1 || recs[0].src.MessageID != 1 {
		t.Fatalf("仅第一条应完成记录: %+v", recs)
	}
	if len(dedup.statList()) != 1 {
		t.Errorf("丢弃的任务不应计数: %+v", dedup.statList())
	}
	if got := cursorOf(e, 1); got != 1 {
		t.Errorf("cursor 应停在 1（第二条 unresolved）: %d", got)
	}
	if pendingOf(e, 1) != 1 {
		t.Errorf("第二条应保持 unresolved: %d", pendingOf(e, 1))
	}
}

// ---------- 辅助 ----------

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ---------- T3.6：媒体大小上限与临时文件 ----------

// 声明尺寸超上限（settings.media_max_size）→ 不下载、永久终结、计失败（03 §3.9）。
func TestEngineMediaTooLargeSkipsPermanently(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	dl := &fakeDownloader{}
	e, store := newTestEngine(t, []domain.ForwardRule{rule(1, 100, 200)}, snd, dedup, dl, nil)
	p := zeroDelayParams()
	p.MediaMaxSizeBytes = 1000
	e.ApplySettings(p)
	startEngine(t, e)

	m := mediaMsg(100, 5, "big", "video")
	m.Media[0].Size = 9999
	e.HandleNew(context.Background(), m)

	waitSignal(t, store.advanced, "超限终结推进")
	if dl.callCount() != 0 {
		t.Error("超限媒体不应触发下载")
	}
	if snd.callCount() != 0 {
		t.Error("超限媒体不应发送")
	}
	stats := dedup.statList()
	if len(stats) != 1 || stats[0].forwarded {
		t.Errorf("应计一次失败: %+v", stats)
	}
	if got := cursorOf(e, 1); got != 5 {
		t.Errorf("超限应永久终结推进 cursor，实际 %d", got)
	}
}

// 下载器返回刷新后的 MediaRef（真实尺寸/文件名）→ LocalFile.Meta 使用新鲜值。
func TestEngineUsesFreshMetaFromDownloader(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	dl := &fakeDownloader{content: []byte("x"), freshSize: 4242, freshName: "cat.mp4"}
	e, _ := newTestEngine(t, []domain.ForwardRule{rule(1, 100, 200)}, snd, dedup, dl, nil)
	startEngine(t, e)

	e.HandleNew(context.Background(), mediaMsg(100, 6, "fresh", "video"))
	waitSignal(t, dedup.recorded, "媒体转发完成")

	calls := snd.fileCalls()
	if len(calls) != 1 || len(calls[0].files) != 1 {
		t.Fatalf("应一次单文件发送: %+v", calls)
	}
	meta := calls[0].files[0].Meta
	if meta.Size != 4242 || meta.FileName != "cat.mp4" {
		t.Errorf("LocalFile.Meta 应为下载器刷新后的值: %+v", meta)
	}
}

// 启动清理：Run 开始时移除残留的任务级临时目录（崩溃保护，03 §3.9）。
func TestEngineStartupCleansStaleTempDirs(t *testing.T) {
	snd, dedup := &fakeSender{}, newFakeForwarded()
	root := t.TempDir()
	stale := filepath.Join(root, "fwd-stale-123")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "chunk.bin"), []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(root, "other-dir")
	if err := os.MkdirAll(keep, 0o700); err != nil {
		t.Fatal(err)
	}

	e, _ := newTestEngine(t, []domain.ForwardRule{rule(1, 100, 200)}, snd, dedup, nil, nil)
	e.tmpRoot = root
	startEngine(t, e)

	eventually(t, "残留 fwd-* 目录清理", func() bool {
		_, err := os.Stat(stale)
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("非 fwd-* 目录不应被清理: %v", err)
	}
}
