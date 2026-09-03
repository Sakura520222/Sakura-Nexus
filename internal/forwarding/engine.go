package forwarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// ---------- 消费侧最小接口（01 §2.3：接口由消费者定义；platform 实现凭结构类型满足） ----------

// Sender 是出站发送最小接口（03 §3.3 三态发送）。platform/telegram.Outbound 满足。
type Sender interface {
	SendText(ctx context.Context, req domain.SendRequest) (domain.SentMessage, error)
	SendFiles(ctx context.Context, req domain.SendRequest, files []domain.LocalFile) (domain.SentMessage, error)
	ForwardMessages(ctx context.Context, from domain.ChatRef, ids []int64, to domain.ChatRef) (domain.SentMessage, error)
}

// RuleStore 是规则读取与 cursor 持久化。mysql.ForwardRuleRepo 满足。
type RuleStore interface {
	ListEnabled(ctx context.Context) ([]domain.ForwardRule, error)
	AdvanceCursor(ctx context.Context, id int64, cursor int64) error
}

// ForwardedStore 是去重与真实成败统计。mysql.ForwardedRepo 满足。
type ForwardedStore interface {
	Exists(ctx context.Context, src domain.MessageRef, target domain.ChatRef) (bool, error)
	ExistsByContent(ctx context.Context, source, target domain.ChatRef, contentHash string) (bool, error)
	Record(ctx context.Context, src domain.MessageRef, target domain.ChatRef,
		ruleID int64, targetMessageID int64, contentHash string) error
	IncrStats(ctx context.Context, ruleID int64, forwarded bool) error
}

// MediaDownloader 将单条媒体流式下载到 dest，返回刷新后的媒体元数据
// （真实尺寸/文件名——原 MediaRef.Size 对 photo 是占位值；03 §3.3 ②/§1.5）。
// 正式实现在 platform/telegram（T3.6）。超过大小上限返回 domain.ErrMediaTooLarge。
type MediaDownloader interface {
	DownloadMedia(ctx context.Context, m domain.ChannelMessage, media domain.MediaRef, dest string) (domain.MediaRef, error)
}

// ChannelSource 查源频道 username（规则 username 辅助列匹配用）。mysql.ChannelRepo 满足。
type ChannelSource interface {
	Get(ctx context.Context, tgID int64) (domain.Channel, bool, error)
}

// SendFailureKind 是发送失败的引擎侧分类（§1.4 矩阵 + P0 Plan §6 cursor 语义）。
type SendFailureKind int

const (
	// FailureTransient：临时错误（网络/FloodWait 超限/重试耗尽）——不推进 cursor，
	// 消息保持未转发，可由回溯补发恢复。
	FailureTransient SendFailureKind = iota
	// FailurePermanent：永久性失败（消息已被源删除、目标被踢等）——一次性标记
	// terminal，避免 cursor 永久卡死（§6）。
	FailurePermanent
)

// FailureClassifier 判定发送错误类别；nil = 全部 transient（保守：cursor 不越过）。
// gotd tgerr 感知的分类器由接线层注入（领域包不 import gotd，01 §2.2）。
type FailureClassifier func(err error) SendFailureKind

// ---------- 运行时参数（settings.forwarding 的引擎侧快照） ----------

// ForwardingParams 是引擎运行时参数（03 §1.6/§3.4/§3.5）。接线方订阅 settings
// 热更回调后调用 ApplySettings 注入（T2.3 SettingsCenter → app → engine）。
type ForwardingParams struct {
	DefaultDelayMinSec  float64 // 规则未配置延迟时的默认随机延迟下限
	DefaultDelayMaxSec  float64
	AlbumQuietMs        int   // 相册静默窗口
	AlbumHardDeadlineMs int   // 相册硬上限
	MediaMaxSizeBytes   int64 // 单文件大小上限（settings.media_max_size_mb，默认 2GB）
	ContentDedup        bool  // 内容哈希去重（防删帖重发）
	DedupDays           int   // 去重记录保留天数（维护清理）
}

// DefaultForwardingParams 与 config.defaultForwarding 保持一致（01 §6.2 默认值）。
func DefaultForwardingParams() ForwardingParams {
	return ForwardingParams{
		DefaultDelayMinSec:  0.5,
		DefaultDelayMaxSec:  2.0,
		AlbumQuietMs:        450,
		AlbumHardDeadlineMs: 2000,
		MediaMaxSizeBytes:   2 << 30, // 2GB（03 §3.9 默认）
		DedupDays:           30,
	}
}

// ---------- Engine ----------

const (
	// sendQueueCapacity：全局发送队列容量（01 §5.2：阻塞背压，满则等待）。
	sendQueueCapacity = 100
	// sendMaxAttempts：transient 失败的最大尝试次数（§1.4 矩阵上限 3 次）。
	sendMaxAttempts = 3
	// albumFlushInterval：相册 FlushDue 驱动周期（quiet 窗口的检测精度）。
	albumFlushInterval = 50 * time.Millisecond
)

// sendRetryBackoff：transient 重试间退避（§1.4 指数退避：3 次尝试 = 2 次间隔）。
var sendRetryBackoff = [sendMaxAttempts - 1]time.Duration{1 * time.Second, 2 * time.Second}

// sendTask 是一个待发送单元：单条消息或相册整组（对齐一条规则）。
type sendTask struct {
	rule        domain.ForwardRule
	msgs        []domain.ChannelMessage
	view        FilterView
	contentHash string // AggregateText 的 sha256 hex（Record 携带；content_dedup 比对）
}

// EngineDeps 是引擎装配依赖（app 接线层构造注入）。
type EngineDeps struct {
	Rules        RuleStore
	Dedup        ForwardedStore
	Sender       Sender
	Media        MediaDownloader // 可选；nil 时 copy 模式媒体消息记 error 跳过（T3.6 前置）
	Channels     ChannelSource   // 可选；nil 时规则 username 辅助列不参与匹配
	Classify     FailureClassifier
	RandomSource func() float64 // 可选；nil = math/rand 全局源
	TmpDir       string         // 媒体临时文件根目录；"" = os.TempDir()（T3.6 完善生命周期）
	Log          *slog.Logger
}

// Engine 是转发引擎编排核心（03 §3.1–§3.5）：事件入口 → 相册聚合分支 → 规则匹配 →
// 过滤链 → 去重 → 发送队列（单消费者、随机延迟、FloodWait/重试矩阵）→ 真实成败
// 统计 → contiguous cursor（P0 Plan §6）。
//
// 以结构类型满足 app.Service（Name/Run/Shutdown）。关闭语义：ctx 取消时消费者
// 完成当前任务后退出（其去重/统计/cursor 记账用不可取消 ctx 完成），队列中剩余
// 任务丢弃——消息保持未转发且 cursor 未越过，由回溯补发恢复（§6 durability）。
// Run 出错返回（如规则装载失败）由 supervisor 按 Criticality 处置后可重入。
type Engine struct {
	deps    EngineDeps
	log     *slog.Logger
	tmpRoot string

	queue chan *sendTask
	rand  func() float64
	sleep func(ctx context.Context, d time.Duration) bool // false = ctx 取消

	params atomic.Pointer[ForwardingParams]

	albumMu  sync.Mutex
	album    *AlbumAggregator
	albumCfg AlbumConfig

	rulesMu          sync.Mutex
	rulesCache       []domain.ForwardRule
	hasUsernameRules bool

	trackMu  sync.Mutex
	trackers map[int64]*cursorTracker

	userMu    sync.Mutex
	usernames map[domain.ChatRef]string // 源频道 username 内存缓存（改绑罕见，重启刷新）

	wg sync.WaitGroup
}

func NewEngine(deps EngineDeps) *Engine {
	lg := deps.Log
	if lg == nil {
		lg = slog.Default()
	}
	rnd := deps.RandomSource
	if rnd == nil {
		rnd = rand.Float64
	}
	tmp := deps.TmpDir
	if tmp == "" {
		tmp = filepath.Join(os.TempDir(), "sakura-nexus") // 03 §3.9 默认子目录
	}
	p := DefaultForwardingParams()
	e := &Engine{
		deps:      deps,
		log:       lg,
		tmpRoot:   tmp,
		queue:     make(chan *sendTask, sendQueueCapacity),
		rand:      rnd,
		sleep:     sleepCtx,
		trackers:  map[int64]*cursorTracker{},
		usernames: map[domain.ChatRef]string{},
	}
	e.params.Store(&p)
	e.albumCfg = albumConfigOf(p)
	e.album = NewAlbumAggregator(e.albumCfg, nil)
	return e
}

// sleepCtx 是生产 sleeper：睡 d 后返回 true；ctx 取消返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func albumConfigOf(p ForwardingParams) AlbumConfig {
	return AlbumConfig{QuietMs: p.AlbumQuietMs, HardDeadlineMs: p.AlbumHardDeadlineMs, MaxSize: 10}
}

// ---------- app.Service 结构类型 ----------

func (e *Engine) Name() string { return "forwarding" }

// Run 装载规则、清理残留临时文件并启动单消费者与相册驱动循环，阻塞至 ctx 取消。
func (e *Engine) Run(ctx context.Context) error {
	if err := e.RefreshRules(ctx); err != nil {
		return fmt.Errorf("装载转发规则: %w", err)
	}
	e.cleanupStaleTemp()
	e.wg.Add(2)
	go e.consumeLoop(ctx)
	go e.albumLoop(ctx)
	e.wg.Wait()
	return nil
}

// Shutdown 满足 app.Service 结构；回收在 Run 内完成（ctx 取消 → wg.Wait）。
func (e *Engine) Shutdown(ctx context.Context) error { return nil }

// ---------- 对外入口 ----------

// RefreshRules 重新装载启用规则（Run 启动时 + WebUI 规则变更后由接线方调用）。
func (e *Engine) RefreshRules(ctx context.Context) error {
	rules, err := e.deps.Rules.ListEnabled(ctx)
	if err != nil {
		return err
	}
	hasUsername := false
	for i := range rules {
		if rules[i].SourceUsername != "" {
			hasUsername = true
			break
		}
	}
	e.rulesMu.Lock()
	e.rulesCache, e.hasUsernameRules = rules, hasUsername
	e.rulesMu.Unlock()
	return nil
}

// ApplySettings 原子替换运行时参数快照（settings.forwarding 热更回调注入）。
// 相册窗口参数变化时：先按旧窗口冲刷暂存分组，再以新参数重建聚合器。
func (e *Engine) ApplySettings(p ForwardingParams) {
	e.params.Store(&p)

	cfg := albumConfigOf(p)
	var flushed [][]domain.ChannelMessage
	e.albumMu.Lock()
	if cfg != e.albumCfg {
		flushed = e.album.FlushDue()
		e.albumCfg = cfg
		e.album = NewAlbumAggregator(cfg, nil)
	}
	e.albumMu.Unlock()
	// settings 回调无业务 ctx：冲刷分组走独立背景上下文，完整进入管线
	for _, g := range flushed {
		e.processGroup(context.Background(), g)
	}
}

// HandleNew 是 User NewMessage 事件入口（接线方在 canonical writer 之后调用）。
// 相册消息经聚合分支暂存，整组就绪后进入规则处理；单条消息直接处理。
// 队列满时阻塞（01 §5.2 阻塞背压：转发不允许丢）。
func (e *Engine) HandleNew(ctx context.Context, m domain.ChannelMessage) {
	if m.GroupedID != 0 {
		e.albumMu.Lock()
		group, ready := e.album.Add(m)
		e.albumMu.Unlock()
		if ready { // 集满/硬上限即时 flush
			e.processGroup(ctx, group)
		}
		return
	}
	e.processGroup(ctx, []domain.ChannelMessage{m})
}

// QueueLen 返回当前队列长度（诊断）。
func (e *Engine) QueueLen() int { return len(e.queue) }

// ---------- 内部编排 ----------

func (e *Engine) consumeLoop(ctx context.Context) {
	defer e.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-e.queue:
			if ctx.Err() != nil {
				return // 关闭中不再拉取新任务（select 双就绪随机性兜底）
			}
			e.execute(ctx, t)
		}
	}
}

func (e *Engine) albumLoop(ctx context.Context) {
	defer e.wg.Done()
	t := time.NewTicker(albumFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.drainAlbum(context.Background()) // 相册冲刷走独立 ctx，不随循环节拍取消
		}
	}
}

// drainAlbum 冲刷到期相册分组并送入规则处理（albumLoop 周期驱动）。
func (e *Engine) drainAlbum(ctx context.Context) {
	e.albumMu.Lock()
	groups := e.album.FlushDue()
	e.albumMu.Unlock()
	for _, g := range groups {
		e.processGroup(ctx, g)
	}
}

// processGroup 对一组消息（单条 len=1 或相册整组）逐规则独立处理
// （03 §3.1：命中多条规则 → 各自过滤/去重/发送，单规则失败不影响其他）。
func (e *Engine) processGroup(ctx context.Context, msgs []domain.ChannelMessage) {
	if len(msgs) == 0 {
		return
	}
	chat := msgs[0].Ref.Chat
	for _, rule := range e.rulesSnapshot() {
		if !e.matchRule(ctx, rule, chat) {
			continue
		}
		e.processRule(ctx, rule, msgs)
	}
}

// processRule 是单规则管线：observe → 五列去重 → content 去重 → 过滤链 → 入队。
// 除入队取消（ctx 取消）与存储错误外，各出口均为 terminal 或保持 unresolved。
func (e *Engine) processRule(ctx context.Context, rule domain.ForwardRule, msgs []domain.ChannelMessage) {
	view := BuildFilterView(msgs)
	hash := e.contentHashOf(view.AggregateText)

	e.observeAll(rule, msgs)

	// 五列去重（03 §3.2 ③）：相册任一成员已转发 → 整组跳过（全成员去重语义）
	for i := range msgs {
		exists, err := e.deps.Dedup.Exists(ctx, msgs[i].Ref, rule.Target)
		if err != nil {
			e.log.Error("去重查询失败，消息保持未转发", "rule", rule.ID, "msg", msgs[i].Ref.MessageID, "err", err)
			return // unresolved：cursor 阻挡，backfill 恢复
		}
		if exists {
			e.log.Info("去重跳过（已转发）", "rule", rule.ID, "msg", msgs[i].Ref.MessageID)
			e.finishTerminals(ctx, rule, msgs)
			return
		}
	}

	// 内容去重（content_dedup 开启时防删帖重发，03 §3.5；纯媒体无文本不比对）
	if p := *e.params.Load(); p.ContentDedup && view.AggregateText != "" {
		hit, err := e.deps.Dedup.ExistsByContent(ctx, msgs[0].Ref.Chat, rule.Target, hash)
		if err != nil {
			e.log.Error("内容去重查询失败，消息保持未转发", "rule", rule.ID, "err", err)
			return
		}
		if hit {
			e.log.Info("内容去重跳过（同内容已转发）", "rule", rule.ID)
			e.finishTerminals(ctx, rule, msgs)
			return
		}
	}

	// 过滤链（03 §3.2 ④→⑧；拒绝 = terminal，03 §3.1）
	if ok, reason := ShouldForward(rule, view); !ok {
		e.log.Info("过滤拒绝", "rule", rule.ID, "msg", msgs[0].Ref.MessageID, "reason", reason)
		e.finishTerminals(ctx, rule, msgs)
		return
	}

	// 入队（容量 100 阻塞背压；ctx 取消则放弃，消息保持 unresolved）
	select {
	case e.queue <- &sendTask{rule: rule, msgs: msgs, view: view, contentHash: hash}:
	case <-ctx.Done():
		e.log.Warn("入队前 ctx 取消，消息保持未转发", "rule", rule.ID, "msg", msgs[0].Ref.MessageID)
	}
}

// execute 是消费者侧执行：随机延迟 → 发送（重试矩阵）→ Record/统计/cursor。
// 发送结局后的记账用不可取消 ctx（WithoutCancel）——已发生的发送必须完成记录。
func (e *Engine) execute(ctx context.Context, t *sendTask) {
	// 每规则随机延迟（03 §3.4：uniform(delay_min, delay_max)，发送前入睡）
	if !e.sleep(ctx, e.delayFor(t.rule)) {
		return // ctx 取消：丢弃任务，unresolved 保持（backfill 恢复）
	}

	sent, err := e.sendWithRetry(ctx, t)
	pctx := context.WithoutCancel(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // 关闭途中：不计失败，unresolved 保持
		}
		e.log.Error("转发失败（transient：cursor 停留，待回溯补发恢复）",
			"rule", t.rule.ID, "msg", t.msgs[0].Ref.MessageID, "err", err)
		if err := e.deps.Dedup.IncrStats(pctx, t.rule.ID, false); err != nil {
			e.log.Error("失败计数写入失败", "rule", t.rule.ID, "err", err)
		}
		if e.classifyFailure(err) == FailurePermanent {
			e.log.Error("永久性失败：标记 terminal（§6 防卡死）", "rule", t.rule.ID, "err", err)
			e.finishTerminals(pctx, t.rule, t.msgs)
		}
		return
	}

	// 成功：全成员写去重记录（相册逐条补记，03 §1.6 全成员去重）
	for i := range t.msgs {
		if err := e.deps.Dedup.Record(pctx, t.msgs[i].Ref, t.rule.Target,
			t.rule.ID, sent.MessageID, t.contentHash); err != nil {
			// 已发送：仍按成功终结（cursor 已越过不会重发；记录缺失仅削弱后续去重）
			e.log.Error("去重记录写入失败（消息已发送，按成功终结）",
				"rule", t.rule.ID, "msg", t.msgs[i].Ref.MessageID, "err", err)
		}
	}
	if err := e.deps.Dedup.IncrStats(pctx, t.rule.ID, true); err != nil {
		e.log.Error("成功计数写入失败", "rule", t.rule.ID, "err", err)
	}
	e.finishTerminals(pctx, t.rule, t.msgs)
}

// sendWithRetry 按 §1.4 矩阵执行发送：transient 重试（指数退避，上限 3 次尝试）；
// permanent 单次尝试即返回。ctx 取消时中止并返回 ctx 错误。
func (e *Engine) sendWithRetry(ctx context.Context, t *sendTask) (domain.SentMessage, error) {
	var lastErr error
	for attempt := 0; attempt < sendMaxAttempts; attempt++ {
		if attempt > 0 {
			if !e.sleep(ctx, sendRetryBackoff[attempt-1]) {
				return domain.SentMessage{}, ctx.Err()
			}
		}
		sent, err := e.sendOnce(ctx, t)
		if err == nil {
			return sent, nil
		}
		lastErr = err
		if e.classifyFailure(err) == FailurePermanent {
			return domain.SentMessage{}, err
		}
	}
	return domain.SentMessage{}, lastErr
}

// sendOnce 按三态发送分派（03 §3.3）：copy_mode=forward / 媒体 / 纯文本。
func (e *Engine) sendOnce(ctx context.Context, t *sendTask) (domain.SentMessage, error) {
	switch {
	case t.rule.CopyMode == "forward":
		return e.deps.Sender.ForwardMessages(ctx, t.msgs[0].Ref.Chat, sourceIDs(t.msgs), t.rule.Target)
	case hasMedia(t.msgs):
		return e.sendAsFiles(ctx, t)
	default:
		return e.deps.Sender.SendText(ctx, domain.SendRequest{
			Chat:     t.rule.Target,
			Text:     t.view.AggregateText,
			Entities: t.msgs[0].Entities, // 转发复制：原 entities 透传
		})
	}
}

// sendAsFiles 下载媒体到任务级临时目录并发送；目录随任务结束即删（03 §3.9 即时删除）。
// 下载前按声明尺寸预检上限；下载器回传刷新后的元数据（真实尺寸/文件名）供上传使用。
// T3.7/T3.8 的 AI 改写与底栏将在此处的 caption 构造点接入（改写后文本 + 底栏）。
func (e *Engine) sendAsFiles(ctx context.Context, t *sendTask) (domain.SentMessage, error) {
	if e.deps.Media == nil {
		return domain.SentMessage{}, fmt.Errorf("媒体下载器未接入（T3.6 前 copy 模式媒体消息不发送）")
	}
	maxSize := e.params.Load().MediaMaxSizeBytes
	for i := range t.msgs {
		for _, media := range t.msgs[i].Media {
			if maxSize > 0 && media.Size > maxSize {
				return domain.SentMessage{}, fmt.Errorf("%w: msg=%d %s 声明 %d 字节 > 上限 %d",
					domain.ErrMediaTooLarge, t.msgs[i].Ref.MessageID, media.Key, media.Size, maxSize)
			}
		}
	}
	dir, err := os.MkdirTemp(e.tmpRoot, "fwd-*")
	if err != nil {
		return domain.SentMessage{}, fmt.Errorf("创建媒体临时目录: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	var files []domain.LocalFile
	for i := range t.msgs {
		for _, media := range t.msgs[i].Media {
			dest := tempMediaPath(dir, t.msgs[i], media)
			fresh, err := e.deps.Media.DownloadMedia(ctx, t.msgs[i], media, dest)
			if err != nil {
				return domain.SentMessage{}, fmt.Errorf("下载媒体 msg=%d %s: %w", t.msgs[i].Ref.MessageID, media.Key, err)
			}
			files = append(files, domain.LocalFile{Path: dest, Meta: fresh})
		}
	}
	return e.deps.Sender.SendFiles(ctx, domain.SendRequest{
		Chat:     t.rule.Target,
		Caption:  t.view.AggregateText,
		Entities: t.msgs[0].Entities, // caption 实体随首成员
	}, files)
}

// cleanupStaleTemp 移除残留的任务级临时目录（fwd-*，崩溃保护；03 §3.9 启动清理）。
func (e *Engine) cleanupStaleTemp() {
	entries, err := os.ReadDir(e.tmpRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			e.log.Warn("读取媒体临时目录失败", "dir", e.tmpRoot, "err", err)
		}
		return
	}
	n := 0
	for _, ent := range entries {
		if ent.IsDir() && strings.HasPrefix(ent.Name(), "fwd-") {
			if err := os.RemoveAll(filepath.Join(e.tmpRoot, ent.Name())); err == nil {
				n++
			} else {
				e.log.Warn("清理残留临时目录失败", "dir", ent.Name(), "err", err)
			}
		}
	}
	if n > 0 {
		e.log.Info("已清理崩溃残留的媒体临时目录", "count", n)
	}
}

func (e *Engine) rulesSnapshot() []domain.ForwardRule {
	e.rulesMu.Lock()
	defer e.rulesMu.Unlock()
	return append([]domain.ForwardRule(nil), e.rulesCache...)
}

func (e *Engine) matchRule(ctx context.Context, rule domain.ForwardRule, chat domain.ChatRef) bool {
	if rule.Source == chat { // ChatRef 精确匹配快路径
		return true
	}
	return MatchSource(rule, chat, e.lookupUsername(ctx, chat))
}

// lookupUsername 返回源频道 username（内存缓存；无 username 规则或未注入
// ChannelSource 时恒为空，避免热路径数据库查询）。
func (e *Engine) lookupUsername(ctx context.Context, chat domain.ChatRef) string {
	e.rulesMu.Lock()
	has := e.hasUsernameRules
	e.rulesMu.Unlock()
	if !has {
		return ""
	}
	e.userMu.Lock()
	cached, ok := e.usernames[chat]
	e.userMu.Unlock()
	if ok {
		return cached
	}
	username := ""
	if e.deps.Channels != nil {
		if ch, found, err := e.deps.Channels.Get(ctx, chat.ID); err != nil {
			e.log.Error("查询频道 username 失败", "chat", chat, "err", err)
		} else if found {
			username = ch.Username
		}
	}
	e.userMu.Lock()
	e.usernames[chat] = username
	e.userMu.Unlock()
	return username
}

// observeAll 将一组消息标记为该规则的 unresolved（进入处理）。
func (e *Engine) observeAll(rule domain.ForwardRule, msgs []domain.ChannelMessage) {
	e.trackMu.Lock()
	defer e.trackMu.Unlock()
	tr := e.trackerForLocked(rule)
	for i := range msgs {
		tr.observe(msgs[i].Ref.MessageID)
	}
}

// finishTerminals 标记一组消息终结并尝试推进 cursor（推进时持久化 AdvanceCursor；
// 持久化失败仅记日志——内存 cursor 已前进，重启后由 backfill 依库内水位收敛）。
func (e *Engine) finishTerminals(ctx context.Context, rule domain.ForwardRule, msgs []domain.ChannelMessage) {
	e.trackMu.Lock()
	tr := e.trackerForLocked(rule)
	moved := false
	for i := range msgs {
		if _, m := tr.terminal(msgs[i].Ref.MessageID); m {
			moved = true
		}
	}
	advanced := tr.current() // 以最终位置持久化（末员可能被其他 unresolved 阻挡）
	e.trackMu.Unlock()
	if moved {
		if err := e.deps.Rules.AdvanceCursor(ctx, rule.ID, advanced); err != nil {
			e.log.Error("cursor 持久化失败", "rule", rule.ID, "cursor", advanced, "err", err)
		}
	}
}

func (e *Engine) trackerForLocked(rule domain.ForwardRule) *cursorTracker {
	if tr, ok := e.trackers[rule.ID]; ok {
		return tr
	}
	tr := newCursorTracker(rule.LastMessageID)
	e.trackers[rule.ID] = tr
	return tr
}

// delayFor 计算规则随机延迟：规则区间非法/未配置时回落 settings 默认（03 §3.4）。
func (e *Engine) delayFor(rule domain.ForwardRule) time.Duration {
	p := *e.params.Load()
	minS, maxS := rule.DelayMinSec, rule.DelayMaxSec
	if minS <= 0 || maxS < minS {
		minS, maxS = p.DefaultDelayMinSec, p.DefaultDelayMaxSec
	}
	secs := minS
	if span := maxS - minS; span > 0 {
		secs += span * e.rand()
	}
	return time.Duration(secs * float64(time.Second))
}

func (e *Engine) classifyFailure(err error) SendFailureKind {
	if errors.Is(err, domain.ErrMediaTooLarge) {
		return FailurePermanent // 内建哨兵：策略性永久失败（03 §3.9）
	}
	if e.deps.Classify == nil {
		return FailureTransient
	}
	return e.deps.Classify(err)
}

func (e *Engine) contentHashOf(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// hasMedia 判定组内是否含媒体（媒体路径优先于纯文本路径，03 §3.3）。
func hasMedia(msgs []domain.ChannelMessage) bool {
	for i := range msgs {
		if len(msgs[i].Media) > 0 {
			return true
		}
	}
	return false
}

func sourceIDs(msgs []domain.ChannelMessage) []int64 {
	ids := make([]int64, len(msgs))
	for i := range msgs {
		ids[i] = msgs[i].Ref.MessageID
	}
	return ids
}

// tempMediaPath 生成媒体临时文件路径（03 §3.9：文件名带 chat_id/message_id 便于排查）。
func tempMediaPath(root string, m domain.ChannelMessage, media domain.MediaRef) string {
	return filepath.Join(root, fmt.Sprintf("%d_%d_%s", m.Ref.Chat.ID, m.Ref.MessageID, media.Key))
}
