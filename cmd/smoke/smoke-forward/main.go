// Command smoke-forward 是 GATE-2 端到端冒烟（P0 Plan §5 Phase 3；同批覆盖
// T3.4 单条发送与 T3.6 媒体下载的 S 验证）：
//
//	真实源 →（updates Manager → dispatcher → engine 规则/过滤/去重/队列）
//	→ copy 模式媒体下载 → Bot 出站 → 真实目标
//
// 三 case：① 文本（entities 透传 + 默认底栏源链接）② 单媒体（下载/上传保真）
// ③ 相册（聚合/整组重建 + forwarded_messages 全成员 dedup 查库）。
// 目标侧经 messages.getHistory 逐 case 核验真实收到。
//
// 频道策略：默认自建一对临时广播频道（源/目标），结束删除清理；-keep 保留排查。
// Bot 由冒烟自动登录（TELEGRAM_BOT_TOKEN）并提升为目标频道管理员。
//
// 前置：.env 完整；userbot 已登录（go run ./cmd/sakura-nexus login-user）。
// 手动执行（S 类验证，不进 CI）：go run ./cmd/smoke/smoke-forward
package main

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jmoiron/sqlx"

	"github.com/Sakura520222/Sakura-Nexus/internal/config"
	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
	"github.com/Sakura520222/Sakura-Nexus/internal/forwarding"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/mysql"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/telegram"
)

func main() {
	keep := flag.Bool("keep", false, "保留冒烟创建的临时频道与规则（排查用）；默认删除清理")
	timeout := flag.Duration("timeout", 3*time.Minute, "整体冒烟超时")
	flag.Parse()

	env, err := config.Load()
	if err != nil {
		fail("配置加载失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	lg := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

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

	failed := &failureFlag{}
	sink := &engineSink{}

	user, manager := telegram.SetupUserUpdates(int(env.TelegramAPIID), env.TelegramAPIHash,
		mysql.NewSessionStorage(db, "user"),
		telegram.UpdatesConfig{
			State: mysql.NewStateStorage(db, "user"),
			Peers: mysql.NewPeerStorage(db, "user"),
			Sink:  sink,
			Log:   lg,
		})

	// Bot：登录后保持连接——引擎出站经本客户端进行（第二阶段 T3.4 S 冒烟同批完成）。
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
			<-ctx.Done() // 保持连接供引擎发送
			return nil
		}); err != nil && ctx.Err() == nil {
			failed.set("bot 客户端: %v", err)
			cancel()
		}
	}()

	sm := &smoke{
		db:        db,
		keep:      *keep,
		failed:    failed,
		cancel:    cancel,
		casesDone: make(chan struct{}),
	}

	err = user.Run(ctx, func(ctx context.Context) error {
		// 提前失败路径（未登录/建频道失败等）也尝试清理已建现场。
		defer func() { sm.doCleanup() }()

		authorized, err := user.Authorized(ctx)
		if err != nil {
			return err
		}
		if !authorized {
			fmt.Println("✗ 尚未登录：请先运行 go run ./cmd/sakura-nexus login-user")
			failed.set("userbot 未登录")
			cancel()
			return nil
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

		// ① 自建临时源/目标广播频道（真实频道；默认结束删除）。
		// 先清扫历史失败运行遗留的孤儿冒烟频道（本冒烟专属标题前缀）。
		if !*keep {
			if n, err := sweepSmokeChannels(ctx, api); err != nil {
				fmt.Printf("⚠ 清扫遗留冒烟频道: %v\n", err)
			} else if n > 0 {
				fmt.Printf("✓ 已清扫 %d 个遗留冒烟频道\n", n)
			}
		}
		stamp := time.Now().Format("0102-150405")
		src, err := createChannel(ctx, api, "Sakura-Nexus Smoke Src "+stamp)
		if err != nil {
			return fmt.Errorf("创建源频道: %w", err)
		}
		sm.src = src // 即时登记：部分失败路径的清理依赖
		tgt, err := createChannel(ctx, api, "Sakura-Nexus Smoke Dst "+stamp)
		if err != nil {
			return fmt.Errorf("创建目标频道: %w", err)
		}
		sm.tgt = tgt
		fmt.Printf("✓ 临时频道: 源=%d 目标=%d\n", src.ID, tgt.ID)

		// ② Bot → 目标频道管理员（获得发帖权）。
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
			return fmt.Errorf("提升 bot 为目标频道管理员: %w", err)
		}
		fmt.Println("✓ Bot 已为目标频道管理员")

		// ③ 规则（copy 模式：媒体走下载/上传保真路径，03 §3.3）。
		ruleID, err := mysql.NewForwardRuleRepo(db).Create(ctx, domain.ForwardRule{
			Name:     "smoke-forward",
			Source:   domain.NewChatRef(domain.PeerChannel, src.ID),
			Target:   domain.NewChatRef(domain.PeerChannel, tgt.ID),
			Enabled:  true,
			CopyMode: "copy",
		})
		if err != nil {
			return fmt.Errorf("写入冒烟规则: %w", err)
		}
		sm.ruleID = ruleID

		// ④ 引擎装配：userbot 侧静态 peer 表（源/目标由创建响应直接持有）；
		// bot 侧解析目标频道 access_hash。
		userPeers := staticPeers{src.ID: peerOf(src), tgt.ID: peerOf(tgt)}
		botPeers, how := resolveBotTarget(ctx, bot.Raw().API(), tgt.ID)
		fmt.Printf("✓ Bot 解析目标频道: %s\n", how)

		engine := forwarding.NewEngine(forwarding.EngineDeps{
			Rules:        mysql.NewForwardRuleRepo(db),
			Dedup:        mysql.NewForwardedRepo(db),
			Sender:       telegram.NewOutbound(bot.Raw(), botPeers, nil),
			Media:        telegram.NewMediaDownloader(user, userPeers, 2<<30, lg),
			AssistantBot: botSelf.Username,
			Classify:     classifyFailure,
			TmpDir:       env.MediaTmpDir,
			Log:          lg,
		})
		sink.engine.Store(engine)
		// 先同步装载规则：确保 manager 启动后到来的首条消息即有规则可命中
		// （engine.Run 内的再次装载是幂等的）。
		if err := engine.RefreshRules(ctx); err != nil {
			return fmt.Errorf("装载冒烟规则: %w", err)
		}
		go func() {
			if err := engine.Run(ctx); err != nil {
				failed.set("引擎: %v", err)
				cancel()
			}
		}()

		return manager.Run(ctx, user.Raw().API(), userSelf.ID, updates.AuthOptions{
			OnStart: func(context.Context) {
				fmt.Println("✓ updates.Manager 已启动")
				sm.started.Store(true)
				go func() {
					defer close(sm.casesDone)
					sm.runCases()
				}()
			},
		})
	})
	if err != nil {
		failed.set("smoke-forward: %v", err)
	}

	if sm.started.Load() {
		select {
		case <-sm.casesDone:
		case <-time.After(150 * time.Second):
			failed.set("用例未在期限内完成（manager 已启动但收尾超时）")
		}
	} else if failed.get() == "" {
		failed.set("updates.Manager 未启动，用例未执行")
	}

	if msg := failed.get(); msg != "" {
		fail("GATE-2 冒烟失败: %s", msg)
	}
	fmt.Println("✓ GATE-2 smoke-forward 通过：文本/单媒体/相册 三 case 端到端 + 相册全成员 dedup 查库")
}

// ---------- 冒烟状态 ----------

type smoke struct {
	db     *sqlx.DB
	api    *tg.Client
	src    *tg.Channel
	tgt    *tg.Channel
	ruleID int64
	keep   bool
	failed *failureFlag
	cancel context.CancelFunc

	started   atomic.Bool
	cleaned   atomic.Bool
	casesDone chan struct{}

	textMarker   string
	textSrcID    int64
	mediaCaption string
	mediaSrcID   int64
	albumSrcIDs  []int64
}

// runCases 执行三 case 与目标侧核验，随后清理现场并结束进程（cancel）。
// 使用独立后台 ctx（自带预算）：收尾不随外部取消中断。
func (s *smoke) runCases() {
	cctx, cc := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cc()

	ok := s.caseText(cctx)
	ok = s.caseMedia(cctx) && ok
	ok = s.caseAlbum(cctx) && ok
	if ok {
		ok = s.verifyTarget(cctx)
	}
	s.verifyStats(cctx) // 记录性质，不参与 GATE 判定
	if !ok {
		s.failed.set("转发 case 未全部成立（详见上方 ✗ 输出与引擎日志）")
	}
	s.doCleanup()
	s.cancel()
}

func (s *smoke) caseText(ctx context.Context) bool {
	marker := fmt.Sprintf("GATE2-TEXT-%d", time.Now().UnixNano())
	res, err := retryFlood(ctx, func() (tg.UpdatesClass, error) {
		return s.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     peerOf(s.src),
			Message:  marker,
			Entities: []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 6}}, // 验证 entities 透传
			RandomID: randID(),
		})
	})
	if err != nil {
		s.failCase("① 文本", err)
		return false
	}
	ids := newMessageIDs(res)
	if len(ids) != 1 {
		s.failCase("① 文本", fmt.Errorf("发送响应未含新消息 id: %v", ids))
		return false
	}
	s.textMarker, s.textSrcID = marker, ids[0]
	if !s.waitForwarded(ctx, ids...) {
		s.failCase("① 文本", errors.New("等待去重记录超时（30s）"))
		return false
	}
	fmt.Printf("✓ ① 文本 case: src=%d 已转发\n", ids[0])
	return true
}

func (s *smoke) caseMedia(ctx context.Context) bool {
	caption := fmt.Sprintf("GATE2-MEDIA-%d", time.Now().UnixNano())
	file, err := uploadPhoto(ctx, s.api, "smoke-media.jpg", jpegPhoto(color.RGBA{R: 220, G: 60, B: 60, A: 255}))
	if err != nil {
		s.failCase("② 单媒体", err)
		return false
	}
	res, err := retryFlood(ctx, func() (tg.UpdatesClass, error) {
		return s.api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer:     peerOf(s.src),
			Media:    &tg.InputMediaUploadedPhoto{File: file},
			Message:  caption,
			RandomID: randID(),
		})
	})
	if err != nil {
		s.failCase("② 单媒体", err)
		return false
	}
	ids := newMessageIDs(res)
	if len(ids) != 1 {
		s.failCase("② 单媒体", fmt.Errorf("发送响应未含新消息 id: %v", ids))
		return false
	}
	s.mediaCaption, s.mediaSrcID = caption, ids[0]
	if !s.waitForwarded(ctx, ids...) {
		s.failCase("② 单媒体", errors.New("等待去重记录超时（30s）"))
		return false
	}
	fmt.Printf("✓ ② 单媒体 case: src=%d 已转发（下载/上传保真路径）\n", ids[0])
	return true
}

func (s *smoke) caseAlbum(ctx context.Context) bool {
	colors := []color.RGBA{
		{R: 40, G: 180, B: 90, A: 255},
		{R: 50, G: 90, B: 220, A: 255},
		{R: 220, G: 180, B: 40, A: 255},
	}
	items := make([]tg.InputSingleMedia, 0, len(colors))
	for i, c := range colors {
		file, err := uploadPhoto(ctx, s.api, fmt.Sprintf("smoke-album-%d.jpg", i), jpegPhoto(c))
		if err != nil {
			s.failCase("③ 相册", err)
			return false
		}
		// 相册成员先经 messages.uploadMedia 注册为服务端媒体（与 outbound 相册
		// 路径一致；裸 InputMediaUploadedPhoto* 成组 400 MEDIA_INVALID）。
		up, err := retryFlood(ctx, func() (tg.MessageMediaClass, error) {
			return s.api.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{
				Peer:  peerOf(s.src),
				Media: &tg.InputMediaUploadedPhoto{File: file},
			})
		})
		if err != nil {
			s.failCase("③ 相册", err)
			return false
		}
		media, err := telegram.RegisteredInputMedia(up)
		if err != nil {
			s.failCase("③ 相册", err)
			return false
		}
		items = append(items, tg.InputSingleMedia{
			Media:    media,
			RandomID: randID(),
		})
	}
	// 单次 sendMultiMedia = Telegram 侧自动成一个相册（GroupedID 由服务端分配）。
	res, err := retryFlood(ctx, func() (tg.UpdatesClass, error) {
		return s.api.MessagesSendMultiMedia(ctx, &tg.MessagesSendMultiMediaRequest{
			Peer:       peerOf(s.src),
			MultiMedia: items,
		})
	})
	if err != nil {
		s.failCase("③ 相册", err)
		return false
	}
	ids := newMessageIDs(res)
	if len(ids) != len(colors) {
		s.failCase("③ 相册", fmt.Errorf("发送响应未含 %d 条新消息: %v", len(colors), ids))
		return false
	}
	s.albumSrcIDs = ids
	if !s.waitForwarded(ctx, ids...) {
		s.failCase("③ 相册", errors.New("等待全成员去重记录超时（30s）"))
		return false
	}
	fmt.Printf("✓ ③ 相册 case: src=%v 三成员均已转发（forwarded_messages 全成员 dedup 查库成立）\n", ids)
	return true
}

// verifyTarget 经 userbot 读目标频道最近历史，逐 case 核验真实收到。
func (s *smoke) verifyTarget(ctx context.Context) bool {
	res, err := retryFlood(ctx, func() (tg.MessagesMessagesClass, error) {
		return s.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peerOf(s.tgt), Limit: 16})
	})
	if err != nil {
		s.failCase("目标侧读取", err)
		return false
	}
	byID := map[int64]*tg.Message{}
	groupSize := map[int64]int{}
	for _, m := range historyMessages(res) {
		byID[int64(m.ID)] = m
		if m.GroupedID != 0 {
			groupSize[m.GroupedID]++
		}
	}
	ok := true

	// ① 文本：正文 + 默认底栏源链接（私有频道 t.me/c/{裸id}/{msg}）+ bold entity 透传。
	tID, err := s.targetMsgID(ctx, s.textSrcID)
	if err != nil {
		s.failCase("① 文本", err)
		ok = false
	} else if tm := byID[tID]; tm == nil {
		s.failCase("① 文本", fmt.Errorf("目标消息 %d 不在最近历史内", tID))
		ok = false
	} else {
		want := fmt.Sprintf("https://t.me/c/%d/%d", s.src.ID, s.textSrcID)
		bold := false
		for _, e := range tm.Entities {
			if b, isB := e.(*tg.MessageEntityBold); isB && b.Offset == 0 && b.Length == 6 {
				bold = true
			}
		}
		switch {
		case !strings.Contains(tm.Message, s.textMarker):
			s.failCase("① 文本", fmt.Errorf("目标消息缺正文 %q（实际 %q）", s.textMarker, tm.Message))
			ok = false
		case !strings.Contains(tm.Message, want):
			s.failCase("① 文本", fmt.Errorf("目标消息缺默认底栏源链接 %s（实际 %q）", want, tm.Message))
			ok = false
		case !bold:
			s.failCase("① 文本", errors.New("目标消息未透传 bold entity"))
			ok = false
		default:
			fmt.Printf("✓ ① 文本验证: 目标 msg=%d 含正文/底栏链接/bold entity\n", tID)
		}
	}

	// ② 单媒体：目标消息为照片且 caption 保真。
	mID, err := s.targetMsgID(ctx, s.mediaSrcID)
	if err != nil {
		s.failCase("② 单媒体", err)
		ok = false
	} else if mm := byID[mID]; mm == nil {
		s.failCase("② 单媒体", fmt.Errorf("目标消息 %d 不在最近历史内", mID))
		ok = false
	} else {
		_, isPhoto := mm.Media.(*tg.MessageMediaPhoto)
		switch {
		case !isPhoto:
			s.failCase("② 单媒体", fmt.Errorf("目标消息媒体不是照片: %T", mm.Media))
			ok = false
		case !strings.Contains(mm.Message, s.mediaCaption):
			s.failCase("② 单媒体", fmt.Errorf("目标消息缺 caption %q（实际 %q）", s.mediaCaption, mm.Message))
			ok = false
		default:
			fmt.Printf("✓ ② 单媒体验证: 目标 msg=%d 为照片且 caption 保真\n", mID)
		}
	}

	// ③ 相册：目标消息成组（GroupedID）且组内成员 = 3（整组重建）。
	aID, err := s.targetMsgID(ctx, s.albumSrcIDs[0])
	if err != nil {
		s.failCase("③ 相册", err)
		ok = false
	} else if am := byID[aID]; am == nil {
		s.failCase("③ 相册", fmt.Errorf("目标消息 %d 不在最近历史内", aID))
		ok = false
	} else {
		switch {
		case am.GroupedID == 0:
			s.failCase("③ 相册", errors.New("目标消息未成组（相册重建失败）"))
			ok = false
		case groupSize[am.GroupedID] != len(s.albumSrcIDs):
			s.failCase("③ 相册", fmt.Errorf("目标相册组成员 %d ≠ %d", groupSize[am.GroupedID], len(s.albumSrcIDs)))
			ok = false
		default:
			fmt.Printf("✓ ③ 相册验证: 目标 msg=%d grouped=%d 组内成员 %d\n",
				aID, am.GroupedID, groupSize[am.GroupedID])
		}
	}
	return ok
}

func (s *smoke) verifyStats(ctx context.Context) {
	var fwd, fl int
	if err := s.db.GetContext(ctx, &fwd,
		`SELECT COALESCE(SUM(forwarded_count), 0) FROM forwarding_stats WHERE rule_id = ?`, s.ruleID); err != nil {
		fmt.Printf("⚠ forwarding_stats 读取: %v\n", err)
		return
	}
	_ = s.db.GetContext(ctx, &fl,
		`SELECT COALESCE(SUM(failed_count), 0) FROM forwarding_stats WHERE rule_id = ?`, s.ruleID)
	if fwd == 3 && fl == 0 {
		fmt.Printf("✓ forwarding_stats: forwarded=%d failed=%d（相册按 1 计）\n", fwd, fl)
	} else {
		fmt.Printf("⚠ forwarding_stats: forwarded=%d failed=%d（期望 3/0，不参与判定，供排查）\n", fwd, fl)
	}
}

// doCleanup 删除冒烟规则与临时频道（幂等；-keep 保留现场）。API 调用走独立
// ctx：外部取消（如整体超时）后仍尽力清理。
func (s *smoke) doCleanup() {
	if s.cleaned.Swap(true) {
		return
	}
	if s.src == nil && s.tgt == nil && s.ruleID == 0 {
		return
	}
	if s.keep {
		fmt.Printf("… -keep: 保留 源=%d 目标=%d 规则=%d（请手动清理）\n",
			derefID(s.src), derefID(s.tgt), s.ruleID)
		return
	}
	cctx, cc := context.WithTimeout(context.WithoutCancel(context.Background()), 30*time.Second)
	defer cc()
	if s.ruleID != 0 {
		if _, err := s.db.ExecContext(cctx, `DELETE FROM forward_rules WHERE id = ?`, s.ruleID); err != nil {
			fmt.Printf("⚠ 删除冒烟规则 %d: %v\n", s.ruleID, err)
		}
	}
	for _, ch := range []*tg.Channel{s.src, s.tgt} {
		if ch == nil {
			continue
		}
		if _, err := retryFlood(cctx, func() (tg.UpdatesClass, error) {
			return s.api.ChannelsDeleteChannel(cctx, inputChannel(ch))
		}); err != nil {
			fmt.Printf("⚠ 删除临时频道 %d: %v\n", ch.ID, err)
		}
	}
	fmt.Println("✓ 已清理临时频道与冒烟规则")
}

func (s *smoke) failCase(name string, err error) {
	fmt.Printf("✗ %s 失败: %v\n", name, err)
}

// waitForwarded 轮询 forwarded_messages 直至 ids 全部落库（引擎记账完成）。
func (s *smoke) waitForwarded(ctx context.Context, ids ...int64) bool {
	if len(ids) == 0 {
		return false
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)+2)
	args = append(args, s.ruleID, s.src.ID)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	q := fmt.Sprintf(`SELECT COUNT(*) FROM forwarded_messages
		WHERE rule_id = ? AND source_chat_id = ? AND source_message_id IN (%s)`,
		strings.Join(ph, ","))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := s.db.GetContext(ctx, &n, q, args...); err == nil && n == len(ids) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false
}

// targetMsgID 查源消息对应的转发落库目标消息 id。
func (s *smoke) targetMsgID(ctx context.Context, srcID int64) (int64, error) {
	var id int64
	if err := s.db.GetContext(ctx, &id,
		`SELECT target_message_id FROM forwarded_messages WHERE rule_id = ? AND source_message_id = ?`,
		s.ruleID, srcID); err != nil {
		return 0, fmt.Errorf("查 target_message_id（src=%d）: %w", srcID, err)
	}
	return id, nil
}

// ---------- 接线适配（正式接线在 T5.1） ----------

// engineSink 把 dispatcher 的领域事件喂入引擎（生产接线顺序：canonical writer
// 之后调 HandleNew；冒烟聚焦转发链路，不落 messages 表）。
type engineSink struct {
	engine atomic.Pointer[forwarding.Engine]
}

func (s *engineSink) OnNew(ctx context.Context, m domain.ChannelMessage) error {
	if e := s.engine.Load(); e != nil {
		e.HandleNew(ctx, m)
	}
	fmt.Printf("✓ NEW %s msg=%d %s\n", m.Ref.Chat, m.Ref.MessageID, excerpt(m.Text))
	return nil
}

func (s *engineSink) OnEdit(_ context.Context, m domain.ChannelMessage) error {
	fmt.Printf("· EDIT %s msg=%d\n", m.Ref.Chat, m.Ref.MessageID)
	return nil
}

func (s *engineSink) OnDelete(_ context.Context, ref domain.MessageRef) error {
	fmt.Printf("· DELETE %s msg=%d\n", ref.Chat, ref.MessageID)
	return nil
}

// staticPeers 是冒烟静态 peer 表：仅注册源/目标频道（创建响应直接持有 access_hash）。
type staticPeers map[int64]*tg.InputPeerChannel

func (p staticPeers) InputPeer(_ context.Context, ref domain.ChatRef) (tg.InputPeerClass, error) {
	if ref.Kind == domain.PeerChannel {
		if peer, ok := p[ref.ID]; ok {
			return peer, nil
		}
	}
	return nil, fmt.Errorf("staticPeers: chat %s 未注册（冒烟仅注册源/目标频道）", ref)
}

// classifyFailure 是冒烟侧最小 gotd 感知分类器（正式映射在 T5.1 接线层，03 §1.4）。
func classifyFailure(err error) forwarding.SendFailureKind {
	for _, code := range []string{
		"CHAT_WRITE_FORBIDDEN", "USER_BANNED_IN_CHANNEL", "CHAT_ADMIN_REQUIRED",
		"CHANNEL_PRIVATE", "MESSAGE_ID_INVALID", "MEDIA_EMPTY", "MEDIA_INVALID",
	} {
		if tgerr.Is(err, code) {
			return forwarding.FailurePermanent
		}
	}
	return forwarding.FailureTransient
}

// ---------- Telegram 辅助 ----------

// retryFlood 在 FLOOD_WAIT 时按服务端要求时长等待后重试（最多 3 次尝试；
// 其余错误直通）。冒烟自建频道/连发用例时 Telegram 限流常见（如建第二频道
// FLOOD_WAIT_5），不值得为此失败整个冒烟。
func retryFlood[T any](ctx context.Context, f func() (T, error)) (T, error) {
	var zero T
	for try := 0; ; try++ {
		v, err := f()
		if err == nil {
			return v, nil
		}
		waited, isFlood := tgerr.AsFloodWait(err)
		if try >= 2 || !isFlood {
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

// sweepSmokeChannels 清扫历史失败运行遗留的冒烟频道（专属标题前缀、仅本人
// 创建的）；返回删除数。部分失败路径（如目标频道创建失败）可能残留孤儿频道。
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

func peerOf(c *tg.Channel) *tg.InputPeerChannel {
	return &tg.InputPeerChannel{ChannelID: c.ID, AccessHash: c.AccessHash}
}

func inputChannel(c *tg.Channel) *tg.InputChannel {
	return &tg.InputChannel{ChannelID: c.ID, AccessHash: c.AccessHash}
}

func createChannel(ctx context.Context, api *tg.Client, title string) (*tg.Channel, error) {
	res, err := retryFlood(ctx, func() (tg.UpdatesClass, error) {
		return api.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
			Title:     title,
			About:     "Sakura-Nexus GATE-2 冒烟临时频道",
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

// resolveBotUser 经 username 解析 bot 的 InputUser（userbot 视角 access_hash）。
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

// resolveBotTarget 从 bot 账号解析目标频道（access_hash 逐账号不同）：
// 优先 channels.getChannels（bot 对其所在频道可用）；失败退回零 hash 直连
// （bot 对其成员频道的已知服务端特例）。
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

func uploadPhoto(ctx context.Context, api *tg.Client, name string, data []byte) (tg.InputFileClass, error) {
	return uploader.NewUploader(api).Upload(ctx, uploader.NewUpload(name, bytes.NewReader(data), int64(len(data))))
}

// newMessageIDs 从频道发送响应（Updates 容器）提取新消息 id。
func newMessageIDs(res tg.UpdatesClass) []int64 {
	u, ok := res.(*tg.Updates)
	if !ok {
		return nil
	}
	var ids []int64
	for _, e := range u.Updates {
		if nm, ok := e.(*tg.UpdateNewChannelMessage); ok {
			if m, ok := nm.Message.(*tg.Message); ok {
				ids = append(ids, int64(m.ID))
			}
		}
	}
	return ids
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

// ---------- 通用辅助 ----------

// failureFlag 记录首个失败原因（并发安全，只记首次）。
type failureFlag struct {
	mu  sync.Mutex
	msg string
}

func (f *failureFlag) set(format string, a ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.msg == "" {
		f.msg = fmt.Sprintf(format, a...)
	}
}

func (f *failureFlag) get() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.msg
}

// jpegPhoto 生成基色渐变 JPEG（1280×720——接近真实照片形态：小尺寸纯色图
// 经 sendMultiMedia 会被服务端相册处理拒绝 400 MEDIA_INVALID，单发可过相册
// 必拒——GATE-2 冒烟两轮实证）。
func jpegPhoto(c color.RGBA) []byte {
	const w, h = 1280, 720
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	scale := func(v uint8, k float64) uint8 {
		x := int(float64(v)*k + 0.5)
		if x > 255 {
			x = 255
		}
		return uint8(x)
	}
	for y := 0; y < h; y++ {
		vk := float64(y) / float64(h-1)
		for x := 0; x < w; x++ {
			hk := float64(x) / float64(w-1)
			img.SetRGBA(x, y, color.RGBA{
				R: scale(c.R, 0.6+0.8*hk),
				G: scale(c.G, 0.6+0.7*vk),
				B: scale(c.B, 0.5+0.9*(1-hk)),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}) // 缓冲区编码无失败路径
	return buf.Bytes()
}

func randID() int64 {
	var b [8]byte
	if _, err := io.ReadFull(crand.Reader, b[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.LittleEndian.Uint64(b[:]))
}

func excerpt(s string) string {
	if len(s) > 60 {
		return `"` + s[:60] + `…"`
	}
	if s == "" {
		return "（媒体/无文本）"
	}
	return `"` + s + `"`
}

func derefID(c *tg.Channel) int64 {
	if c == nil {
		return 0
	}
	return c.ID
}

func fail(format string, a ...any) {
	fmt.Printf("✗ "+format+"\n", a...)
	os.Exit(1)
}
