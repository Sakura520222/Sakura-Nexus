// smoke-rich —— Rich smoke checkpoint（T4.3，非 Gate；07 §1.2）：
// 真实 sendRichMessage 发送 + 不支持/内容超限时降级观察。
//
// 前置：.env 凭据齐备；userbot 已登录（login-user）；bot session 已存在。
// 流程：userbot 自建临时广播频道并提升 bot 为管理员 → 经 Outbound Rich 路由
// 发送三 case（正常 Rich / 长内容分块 / 不可发送内容强制降级）→ userbot
// getHistory 核验落点 → 删除临时频道清理。
//
// 观测面：slog 捕获器识别「rich 发送降级」告警，逐 case 报告实际行走
// （Rich 通道 / MTProto 降级）与 RichCapability 终态。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/Sakura520222/Sakura-Nexus/internal/config"
	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/botapi"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/mysql"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/telegram"
)

func main() {
	keep := flag.Bool("keep", false, "保留冒烟创建的临时频道（排查用）；默认删除清理")
	timeout := flag.Duration("timeout", 6*time.Minute, "整体超时（case③ 前含 45s 限流静默）")
	flag.Parse()

	env, err := config.Load()
	if err != nil {
		fail("配置加载失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	degraded := &degradationLog{inner: slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})}
	lg := slog.New(degraded)

	db, err := mysql.Connect(ctx, mysql.Options{
		Host: env.MySQLHost, Port: env.MySQLPort,
		User: env.MySQLUser, Password: env.MySQLPassword, Database: env.MySQLDatabase,
		MaxOpenConns: env.MySQLMaxOpenConns,
	})
	if err != nil {
		fail("MySQL 连接: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := mysql.MigrateUp(ctx, db.DB); err != nil {
		fail("goose 迁移: %v", err)
	}

	bot := telegram.NewBotClient(int(env.TelegramAPIID), env.TelegramAPIHash, mysql.NewSessionStorage(db, "bot"))
	botReady := make(chan tg.User, 1)
	go func() {
		if err := bot.Run(ctx, func(ctx context.Context) error {
			if err := bot.AuthBot(ctx, env.TelegramBotToken); err != nil {
				return fmt.Errorf("bot 登录: %w", err)
			}
			self, err := bot.VerifySelf(ctx)
			if err != nil {
				return fmt.Errorf("bot 身份校验: %w", err)
			}
			fmt.Printf("✓ Bot: id=%d @%s\n", self.ID, self.Username)
			botReady <- self
			<-ctx.Done()
			return nil
		}); err != nil && ctx.Err() == nil {
			fmt.Printf("✗ bot 客户端: %v\n", err)
			cancel()
		}
	}()

	user := telegram.NewUserClient(int(env.TelegramAPIID), env.TelegramAPIHash,
		mysql.NewSessionStorage(db, "user"))
	sm := &smoke{keep: *keep, degraded: degraded}

	err = user.Run(ctx, func(ctx context.Context) error {
		defer func() { sm.cleanup() }()

		authorized, err := user.Authorized(ctx)
		if err != nil {
			return err
		}
		if !authorized {
			return errors.New("userbot 未登录：请先运行 go run ./cmd/sakura-nexus login-user")
		}
		userSelf, err := user.Self(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("✓ User: id=%d @%s\n", userSelf.ID, userSelf.Username)

		var botSelf tg.User
		select {
		case botSelf = <-botReady:
		case <-ctx.Done():
			return ctx.Err()
		}

		api := user.Raw().API()
		sm.api = api

		// 临时目标频道（Rich 发送目标；默认结束删除）。
		if !*keep {
			if n, err := sweepSmokeChannels(ctx, api); err != nil {
				fmt.Printf("⚠ 清扫遗留冒烟频道: %v\n", err)
			} else if n > 0 {
				fmt.Printf("✓ 已清扫 %d 个遗留冒烟频道\n", n)
			}
		}
		stamp := time.Now().Format("0102-150405")
		tgt, err := createChannel(ctx, api, "Sakura-Nexus Smoke Rich "+stamp)
		if err != nil {
			return fmt.Errorf("创建临时频道: %w", err)
		}
		sm.tgt = tgt
		fmt.Printf("✓ 临时频道: %d\n", tgt.ID)

		botUser, err := resolveBotUser(ctx, api, botSelf)
		if err != nil {
			return fmt.Errorf("解析 bot 账号: %w", err)
		}
		if _, err := retryFlood(ctx, func() (tg.UpdatesClass, error) {
			return api.ChannelsEditAdmin(ctx, &tg.ChannelsEditAdminRequest{
				Channel:     inputChannel(tgt),
				UserID:      botUser,
				AdminRights: tg.ChatAdminRights{PostMessages: true, EditMessages: true, DeleteMessages: true, InviteUsers: true},
				Rank:        "smoke",
			})
		}); err != nil {
			return fmt.Errorf("提升 bot 管理员: %w", err)
		}
		fmt.Println("✓ Bot 已为频道管理员")

		// Bot API 侧成员关系传播探测（MTProto 提权 → Bot API 视图存在秒级延迟；
		// 未传播时 sendRichMessage 403 "bot is not a member"）。
		richProbe := botapi.NewClient(env.TelegramBotToken, botapi.Options{Log: lg})
		chatID := -(1_000_000_000_000 + tgt.ID)
		deadline := time.Now().Add(30 * time.Second)
		for {
			var pong map[string]any
			err := richProbe.Call(ctx, "getChat", map[string]any{"chat_id": chatID}, &pong)
			if err == nil {
				fmt.Println("✓ Bot API 成员关系已传播（getChat 命中）")
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("getChat 传播探测超时: %w", err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}

		botPeers, how := resolveBotTarget(ctx, bot.Raw().API(), tgt.ID)
		fmt.Printf("✓ Bot 解析频道: %s\n", how)

		// Rich 通道：Bot API HTTP 客户端（同一 token，ADR-008）→ Outbound 路由。
		richClient := botapi.NewClient(env.TelegramBotToken, botapi.Options{Log: lg})
		out := telegram.NewOutbound(bot.Raw(), botPeers, richClient, telegram.WithLog(lg))

		if enabled, reason := out.RichCapability(); enabled {
			fmt.Println("✓ RichCapability: 已启用")
		} else {
			fmt.Printf("✓ RichCapability: 禁用（%s）\n", reason)
		}

		cctx, cc := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cc()
		ok := sm.caseRich(cctx, out)
		time.Sleep(6 * time.Second) // bot 频道发帖限流缓冲
		ok = sm.caseLongSplit(cctx, out) && ok
		ok = sm.caseForcedFallback(cctx, out) && ok

		if enabled, reason := out.RichCapability(); !enabled {
			fmt.Printf("· 终态 RichCapability: 禁用（%s）\n", reason)
		} else {
			fmt.Println("· 终态 RichCapability: 仍启用")
		}
		if !ok {
			return errors.New(strings.TrimSpace(sm.failed))
		}
		return nil
	})
	if err != nil {
		fail("Rich smoke checkpoint 失败: %v", err)
	}
	fmt.Println("✓ Rich smoke checkpoint 通过：sendRichMessage 实发 + 降级路径观察完成")
}

// ---------- 冒烟状态 ----------

type smoke struct {
	api      *tg.Client
	tgt      *tg.Channel
	keep     bool
	failed   string
	degraded *degradationLog

	cleanOnce sync.Once
}

func (s *smoke) setFailed(format string, a ...any) {
	s.failed += "\n" + fmt.Sprintf(format, a...)
}

// degradationLog 捕获 richRouter 的降级告警，供逐 case 观测实际行走。
type degradationLog struct {
	inner    slog.Handler
	mu       sync.Mutex
	degCount int
}

func (h *degradationLog) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}
func (h *degradationLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &degradationLog{inner: h.inner.WithAttrs(attrs)}
}
func (h *degradationLog) WithGroup(name string) slog.Handler {
	return &degradationLog{inner: h.inner.WithGroup(name)}
}
func (h *degradationLog) Handle(ctx context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "rich 发送降级") {
		h.mu.Lock()
		h.degCount++
		h.mu.Unlock()
	}
	return h.inner.Handle(ctx, r)
}

func (h *degradationLog) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.degCount
}

// caseRich 正常 Rich 内容：标题/任务清单/表格/粗体 + 唯一 marker。
func (s *smoke) caseRich(ctx context.Context, out *telegram.Outbound) bool {
	marker := fmt.Sprintf("RICHSMOKE-A-%d", time.Now().UnixNano())
	md := strings.Join([]string{
		"# " + marker,
		"",
		"- [x] 引擎 GATE-2 通过",
		"- [ ] Rich checkpoint 观察中",
		"",
		"| 组件 | 状态 |",
		"|---|---|",
		"| botapi | **正常** |",
		"| renderer | **正常** |",
	}, "\n")

	before := s.degraded.count()
	sent, err := retryFlood(ctx, func() (domain.SentMessage, error) {
		return out.SendText(ctx, domain.SendRequest{
			Chat:    domain.NewChatRef(domain.PeerChannel, s.tgt.ID),
			Style:   domain.StyleAuto,
			Content: &domain.MessageContent{Text: md},
		})
	})
	if err != nil {
		s.setFailed("case① Rich 发送失败: %v", err)
		return false
	}
	path := "Rich 通道"
	if s.degraded.count() > before {
		path = "MTProto 降级"
	}
	fmt.Printf("· case① 行走路径: %s sent_id=%d\n", path, sent.MessageID)
	return s.verifySentID(ctx, sent.MessageID, "case①")
}

// caseLongSplit 长内容（3×16000 字符段落 → 2 块）验证 block 切分实发。
func (s *smoke) caseLongSplit(ctx context.Context, out *telegram.Outbound) bool {
	marker := fmt.Sprintf("RICHSMOKE-B-%d", time.Now().UnixNano())
	para := func(tag string) string { return "## " + marker + " " + tag + "\n\n" + strings.Repeat("字", 15900) }
	md := strings.Join([]string{para("p1"), para("p2"), para("p3")}, "\n\n")

	sent, err := retryFlood(ctx, func() (domain.SentMessage, error) {
		return out.SendText(ctx, domain.SendRequest{
			Chat:    domain.NewChatRef(domain.PeerChannel, s.tgt.ID),
			Style:   domain.StyleAuto,
			Content: &domain.MessageContent{Text: md},
		})
	})
	if err != nil {
		s.setFailed("case② 长内容发送失败: %v", err)
		return false
	}
	fmt.Printf("· case② 行走路径: 长内容分块 sent_id=%d\n", sent.MessageID)
	return s.verifySentID(ctx, sent.MessageID, "case②")
}

// caseForcedFallback 17 层嵌套引用（>16 层上限）→ ErrRichUnsendable →
// 确定性地走 fallback 链（03 §2.7），capability 不应被禁用。选深层引用而非
// 单行超限：兜底文本仅一行小消息，不受新频道 bot 连发限流制约。
func (s *smoke) caseForcedFallback(ctx context.Context, out *telegram.Outbound) bool {
	marker := fmt.Sprintf("RICHSMOKE-C-%d", time.Now().UnixNano())
	line := strings.Repeat(">", 17) + " " + marker + " 深层引用（17 层 > 16 上限）"

	before := s.degraded.count()
	sent, err := retryFloodN(ctx, 5, func() (domain.SentMessage, error) {
		return out.SendText(ctx, domain.SendRequest{
			Chat:    domain.NewChatRef(domain.PeerChannel, s.tgt.ID),
			Style:   domain.StyleAuto,
			Content: &domain.MessageContent{Text: line},
		})
	})
	if err != nil {
		s.setFailed("case③ 强制降级发送失败: %v", err)
		return false
	}
	if s.degraded.count() <= before {
		s.setFailed("case③ 预期走降级链（ErrRichUnsendable），但未见降级告警")
		return false
	}
	if enabled, _ := out.RichCapability(); !enabled {
		s.setFailed("case③ 内容超限不应禁用 capability")
		return false
	}
	fmt.Printf("· case③ 行走路径: MTProto 降级（capability 保持）sent_id=%d\n", sent.MessageID)
	return s.verifySentID(ctx, sent.MessageID, "case③")
}

// verifySentID 轮询 userbot 侧 getHistory 核验发送回执 ID 已落到目标频道。
// 注：rich payload 对 gotd v0.161 不可见（未知字段被丢弃，历史文本为空），
// 文本 marker 核验不成立，故按消息 ID 命中判定。
func (s *smoke) verifySentID(ctx context.Context, sentID int64, caseName string) bool {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err := retryFlood(ctx, func() (tg.MessagesMessagesClass, error) {
			return s.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer:  peerOf(s.tgt),
				Limit: 20,
			})
		})
		if err != nil {
			s.setFailed("%s getHistory: %v", caseName, err)
			return false
		}
		for _, m := range historyMessages(res) {
			if int64(m.ID) == sentID {
				fmt.Printf("✓ %s 落点核验: msg_id=%d\n", caseName, m.ID)
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	// 诊断转储：Rich 消息在历史中的真实形态（渲染后文本/实体）。
	res, err := retryFlood(ctx, func() (tg.MessagesMessagesClass, error) {
		return s.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peerOf(s.tgt), Limit: 20})
	})
	if err == nil {
		msgs := historyMessages(res)
		fmt.Printf("· %s 诊断：历史 %d 条\n", caseName, len(msgs))
		for _, m := range msgs {
			excerpt := strings.ReplaceAll(string([]rune(m.Message)[:minInt(60, len([]rune(m.Message)))]), "\n", "⏎")
			fmt.Printf("  · msg_id=%d len=%d entities=%d %q\n", m.ID, len([]rune(m.Message)), len(m.Entities), excerpt)
		}
	}
	s.setFailed("%s 期限内未在目标频道核验到 msg_id=%d", caseName, sentID)
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// cleanup 删除临时频道（-keep 保留现场）。
func (s *smoke) cleanup() {
	s.cleanOnce.Do(func() {
		if s.keep || s.tgt == nil {
			return
		}
		cctx, cc := context.WithTimeout(context.Background(), 30*time.Second)
		defer cc()
		if _, err := retryFlood(cctx, func() (tg.UpdatesClass, error) {
			return s.api.ChannelsDeleteChannel(cctx, inputChannel(s.tgt))
		}); err != nil {
			fmt.Printf("⚠ 清理临时频道 %d 失败: %v\n", s.tgt.ID, err)
			return
		}
		fmt.Printf("✓ 已删除临时频道 %d\n", s.tgt.ID)
	})
}

// ---------- Telegram 辅助（与 smoke-forward 同模式；冒烟入口自包含） ----------

func retryFlood[T any](ctx context.Context, f func() (T, error)) (T, error) {
	return retryFloodN(ctx, 2, f)
}

func retryFloodN[T any](ctx context.Context, maxRetries int, f func() (T, error)) (T, error) {
	var zero T
	for try := 0; ; try++ {
		v, err := f()
		if err == nil {
			return v, nil
		}
		waited, isFlood := tgerr.AsFloodWait(err)
		if try >= maxRetries || !isFlood {
			return zero, err
		}
		wait := waited + time.Second
		fmt.Printf("· FLOOD_WAIT：%v 后重试\n", wait)
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func sweepSmokeChannels(ctx context.Context, api *tg.Client) (int, error) {
	res, err := retryFlood(ctx, func() (tg.MessagesDialogsClass, error) {
		return api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetPeer: &tg.InputPeerEmpty{},
			Limit:      100,
		})
	})
	if err != nil {
		return 0, fmt.Errorf("getDialogs: %w", err)
	}
	var chats []tg.ChatClass
	switch v := res.(type) {
	case *tg.MessagesDialogs:
		chats = v.Chats
	case *tg.MessagesDialogsSlice:
		chats = v.Chats
	}
	n := 0
	for _, c := range chats {
		ch, ok := c.(*tg.Channel)
		if !ok || !strings.HasPrefix(ch.Title, "Sakura-Nexus Smoke") || !ch.Creator {
			continue
		}
		if _, err := retryFlood(ctx, func() (tg.UpdatesClass, error) {
			return api.ChannelsDeleteChannel(ctx, inputChannel(ch))
		}); err != nil {
			return n, fmt.Errorf("删除遗留频道 %d: %w", ch.ID, err)
		}
		n++
	}
	return n, nil
}

func createChannel(ctx context.Context, api *tg.Client, title string) (*tg.Channel, error) {
	res, err := retryFlood(ctx, func() (tg.UpdatesClass, error) {
		return api.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
			Title:     title,
			About:     "Sakura-Nexus Rich smoke checkpoint 临时频道",
			Broadcast: true,
		})
	})
	if err != nil {
		return nil, err
	}
	u, ok := res.(*tg.Updates)
	if !ok {
		return nil, fmt.Errorf("创建 %s: 意外响应 %T", title, res)
	}
	for _, c := range u.Chats {
		if ch, ok := c.(*tg.Channel); ok {
			return ch, nil
		}
	}
	return nil, fmt.Errorf("创建 %s: 响应不含频道实体", title)
}

func resolveBotUser(ctx context.Context, api *tg.Client, botSelf tg.User) (*tg.InputUser, error) {
	if botSelf.Username == "" {
		return nil, errors.New("bot 无 username，无法解析")
	}
	res, err := retryFlood(ctx, func() (*tg.ContactsResolvedPeer, error) {
		return api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: botSelf.Username})
	})
	if err != nil {
		return nil, err
	}
	for _, u := range res.Users {
		if uu, ok := u.(*tg.User); ok && uu.ID == botSelf.ID {
			return &tg.InputUser{UserID: uu.ID, AccessHash: uu.AccessHash}, nil
		}
	}
	return nil, fmt.Errorf("resolveUsername 未返回 bot %d", botSelf.ID)
}

func resolveBotTarget(ctx context.Context, botAPI *tg.Client, channelID int64) (telegram.PeerResolver, string) {
	var hash int64
	res, err := retryFlood(ctx, func() (tg.MessagesChatsClass, error) {
		return botAPI.ChannelsGetChannels(ctx, []tg.InputChannelClass{
			&tg.InputChannel{ChannelID: channelID},
		})
	})
	if err == nil {
		if chats, ok := res.(*tg.MessagesChats); ok {
			for _, c := range chats.Chats {
				if ch, ok := c.(*tg.Channel); ok && ch.ID == channelID {
					hash = ch.AccessHash
				}
			}
		}
	}
	peer := &tg.InputPeerChannel{ChannelID: channelID, AccessHash: hash}
	if hash != 0 {
		return staticPeers{channelID: peer}, fmt.Sprintf("channels.getChannels 命中 access_hash=%d", hash)
	}
	return staticPeers{channelID: peer}, "access_hash=0 直连（getChannels 未回 hash，bot 特例路径）"
}

type staticPeers map[int64]*tg.InputPeerChannel

func (p staticPeers) InputPeer(_ context.Context, ref domain.ChatRef) (tg.InputPeerClass, error) {
	if ref.Kind == domain.PeerChannel {
		if peer, ok := p[ref.ID]; ok {
			return peer, nil
		}
	}
	return nil, fmt.Errorf("staticPeers: chat %s 未注册", ref)
}

func historyMessages(res tg.MessagesMessagesClass) []*tg.Message {
	var raw []tg.MessageClass
	switch v := res.(type) {
	case *tg.MessagesMessages:
		raw = v.Messages
	case *tg.MessagesMessagesSlice:
		raw = v.Messages
	case *tg.MessagesChannelMessages:
		raw = v.Messages
	}
	var out []*tg.Message
	for _, m := range raw {
		if mm, ok := m.(*tg.Message); ok {
			out = append(out, mm)
		}
	}
	return out
}

func peerOf(c *tg.Channel) *tg.InputPeerChannel {
	return &tg.InputPeerChannel{ChannelID: c.ID, AccessHash: c.AccessHash}
}

func inputChannel(c *tg.Channel) *tg.InputChannel {
	return &tg.InputChannel{ChannelID: c.ID, AccessHash: c.AccessHash}
}

func fail(format string, a ...any) {
	fmt.Printf("✗ "+format+"\n", a...)
	os.Exit(1)
}
