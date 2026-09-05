package app

// Production 装配（T5.1 接线收口；01 §1.1 启动序列 2–8）：MySQL → settings →
// platform 构造 → 领域 service 构造（不启动）→ 注册 lifecycle。
// gotd/sqlx/openai-go 全部封在 platform 层，本文件只做组合（01 §2.2）。

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sakura520222/Sakura-Nexus/internal/config"
	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
	"github.com/Sakura520222/Sakura-Nexus/internal/forwarding"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/ai"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/botapi"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/mysql"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/telegram"
	"github.com/Sakura520222/Sakura-Nexus/internal/webapi"
)

// Production 是装配产物：lifecycle App + T5.3 WebAPI 需要的句柄。
type Production struct {
	App *App

	Engine   *forwarding.Engine
	Settings *config.SettingsCenter

	// RequestRestart 供 WebUI restart（exit 75 全链，01 §1.4；T5.3 system API 消费）。
	RequestRestart func()
}

// Assemble 按冻结启动序列构造全部组件并注册（不启动；App.Run 统一启动）。
// MySQL 连接/迁移失败返回错误（CORE，main 以 exit 1 终止）。
func Assemble(ctx context.Context, env *config.Env, lg *slog.Logger) (*Production, error) {
	if lg == nil {
		lg = slog.Default()
	}

	// 2. MySQL：连接池 + 启动即迁移（goose，embed SQL）。
	db, err := mysql.Connect(ctx, mysql.Options{
		Host: env.MySQLHost, Port: env.MySQLPort,
		User: env.MySQLUser, Password: env.MySQLPassword, Database: env.MySQLDatabase,
		MaxOpenConns: env.MySQLMaxOpenConns,
	})
	if err != nil {
		return nil, fmt.Errorf("MySQL 连接: %w", err)
	}
	if err := mysql.MigrateUp(ctx, db.DB); err != nil {
		return nil, fmt.Errorf("MySQL 迁移: %w", err)
	}

	// 3. settings 中心：加载 MySQL settings → 内存快照。
	center := config.NewSettingsCenter(db)
	if err := center.Load(ctx); err != nil {
		return nil, fmt.Errorf("settings 加载: %w", err)
	}

	// 4. platform 构造（唯一触碰具体库的层）。
	ruleRepo := mysql.NewForwardRuleRepo(db)
	dedupRepo := mysql.NewForwardedRepo(db)
	channelRepo := mysql.NewChannelRepo(db)

	// user 侧 state/peers（updates 闭环 + History/Media 解析共用同一 storage 实例）。
	userPeerStorage := mysql.NewPeerStorage(db, "user")
	sink := &sinkHolder{lg: lg}
	user, manager := telegram.SetupUserUpdates(int(env.TelegramAPIID), env.TelegramAPIHash,
		mysql.NewSessionStorage(db, "user"),
		telegram.UpdatesConfig{
			State: mysql.NewStateStorage(db, "user"),
			Peers: userPeerStorage,
			Sink:  sink,
			Log:   lg,
		})
	userPeers := telegram.NewPeerBook(userPeerStorage, user.Raw().API())

	bot := telegram.NewBotClient(int(env.TelegramAPIID), env.TelegramAPIHash, mysql.NewSessionStorage(db, "bot"))
	// Bot 侧 peer 解析走 telegram_peers bot account 行（channel 未命中回源
	// getChannels 并回写，GATE-2 实证路径）。
	botPeers := telegram.NewPeerBook(mysql.NewPeerStorage(db, "bot"), bot.Raw().API())

	richClient := botapi.NewClient(env.TelegramBotToken, botapi.Options{Log: lg})
	outbound := telegram.NewOutbound(bot.Raw(), botPeers, richClient, telegram.WithLog(lg))

	// AI provider：settings.ai 快照构造；热更回调换新实例（Provider 无内部状态）。
	aiHolder := &aiProviderHolder{center: center, lg: lg}
	aiHolder.Store(aiHolder.build())

	// 5. 领域 service 构造（不启动任何 service）。
	var assistantBot atomic.Value // bot 登录后填充（{assistant_bot} 占位符）
	botSvc := telegram.NewBotService(bot, env.TelegramBotToken, func(username string) {
		assistantBot.Store(username)
	}, lg)

	fwdParams := forwardingParamsOf(center.Forwarding())
	engine := forwarding.NewEngine(forwarding.EngineDeps{
		Rules:          ruleRepo,
		Dedup:          dedupRepo,
		Sender:         outbound,
		Media:          telegram.NewMediaDownloader(user, userPeers, fwdParams.MediaMaxSizeBytes, lg),
		Channels:       channelRepo,
		AssistantBotFn: func() string { s, _ := assistantBot.Load().(string); return s },
		Rewriter:       aiRewriter{holder: aiHolder},
		History:        telegram.NewHistory(user, userPeers, lg),
		Classify:       classifySendFailure,
		TmpDir:         env.MediaTmpDir,
		Log:            lg,
	})
	engine.ApplySettings(fwdParams)
	sink.Set(&messageSink{repo: mysql.NewMessageRepository(db), engine: engine, lg: lg})
	userSvc := telegram.NewUserService(user, manager, lg)
	engSvc := &engineService{engine: engine}

	web := webapi.NewServer(env.WebUIHost, env.WebUIPort, lg,
		webapi.WithCredentials(env.WebUIUsername, env.WebUIPassword),
		webapi.WithAuditSink(mysql.NewAuditRepo(db)),
	)

	// 6. 注册 lifecycle（启动序 user→bot→engine→web；关闭逆序 web→engine→bot→user）。
	opts := Options{
		ShutdownTimeout: seconds(env.ShutdownTimeoutSeconds),
		Log:             lg,
	}
	a := New(opts)
	a.Register(userSvc, Degradable)
	a.Register(botSvc, Core) // 01 §1.3：BotClient STRICT——失败 → exit 1
	a.Register(engSvc, Degradable)
	a.Register(web, Core) // WebServer 是普通注册 service，CORE

	// settings 热更订阅（T2.3 → engine；T5.3 WebUI 改 settings 后经 notify 到达）。
	center.Subscribe("forwarding", func(string) {
		engine.ApplySettings(forwardingParamsOf(center.Forwarding()))
	})
	center.Subscribe("ai", func(string) {
		aiHolder.Store(aiHolder.build())
	})

	return &Production{
		App:            a,
		Engine:         engine,
		Settings:       center,
		RequestRestart: a.RequestRestart,
	}, nil
}

// ---------- service 包装与适配（本包内，无基础设施库） ----------

// engineService 把 *forwarding.Engine 适配为 app.Service。
type engineService struct{ engine *forwarding.Engine }

func (s *engineService) Name() string                   { return "forwarding" }
func (s *engineService) Run(ctx context.Context) error  { return s.engine.Run(ctx) }
func (s *engineService) Shutdown(context.Context) error { return nil }

// classifySendFailure 是 gotd 感知分类器的接线层组合（领域包不 import gotd，
// 01 §2.2）：permanent 判定在 platform/telegram（tgerr 全集），此处映射类型。
func classifySendFailure(err error) forwarding.SendFailureKind {
	if telegram.IsPermanentSendError(err) {
		return forwarding.FailurePermanent
	}
	return forwarding.FailureTransient
}

// forwardingParamsOf 把 settings.forwarding 快照转换为引擎运行时参数
// （MB → 字节；其余直映）。
func forwardingParamsOf(s config.ForwardingSettings) forwarding.ForwardingParams {
	p := forwarding.DefaultForwardingParams()
	p.ShowDefaultFooter = s.ShowDefaultFooter
	p.DedupDays = s.DedupDays
	p.ContentDedup = s.ContentDedup
	p.DefaultDelayMinSec = s.DefaultDelayMinSec
	p.DefaultDelayMaxSec = s.DefaultDelayMaxSec
	p.AlbumQuietMs = s.AlbumQuietMs
	p.AlbumHardDeadlineMs = s.AlbumHardDeadlineMs
	p.MediaMaxSizeBytes = int64(s.MediaMaxSizeMB) << 20
	return p
}

// aiProviderHolder 原子持有当前 ai.Provider（settings.ai 热更换实例）。
type aiProviderHolder struct {
	center *config.SettingsCenter
	lg     *slog.Logger
	ptr    atomic.Pointer[ai.Provider]
}

func (h *aiProviderHolder) build() *ai.Provider {
	s := h.center.AI()
	return ai.NewProvider(ai.Config{
		BaseURL:        s.BaseURL,
		APIKey:         s.APIKey,
		RewriteModel:   s.RewriteModel,
		Temperature:    s.Temperature,
		TimeoutSeconds: s.TimeoutSeconds,
	}, h.lg)
}

func (h *aiProviderHolder) Store(p *ai.Provider) { h.ptr.Store(p) }
func (h *aiProviderHolder) Load() *ai.Provider   { return h.ptr.Load() }

// aiRewriter 把 platform/ai.Provider 适配为 forwarding.Rewriter（03 §3.2 ⑧：
// prompt 取 rule.AIPrompt，文本取聚合视图）。改写失败由引擎降级原文（03 §1.4）。
type aiRewriter struct{ holder *aiProviderHolder }

// Rewrite 实现 forwarding.Rewriter。
func (r aiRewriter) Rewrite(ctx context.Context, rule domain.ForwardRule, view forwarding.FilterView) (domain.AIResponse, error) {
	return r.holder.Load().Rewrite(ctx, rule.AIPrompt, view.AggregateText)
}

func seconds(n int) time.Duration {
	if n <= 0 {
		return 30 * time.Second
	}
	return time.Duration(n) * time.Second
}

// sinkHolder 是 UpdatesConfig.Sink 的延迟填充代理：SetupUserUpdates 装配早于
// engine 构造（engine 依赖 user 客户端），Assemble 完成前到达的事件丢弃并告警
// （生产时序下 manager.Run 启动晚于 Assemble 完成，此分支仅为防御）。
type sinkHolder struct {
	mu   sync.Mutex
	sink telegram.MessageSink
	lg   *slog.Logger
}

func (h *sinkHolder) Set(s telegram.MessageSink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sink = s
}

func (h *sinkHolder) delegate() telegram.MessageSink {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sink == nil {
		h.lg.Warn("updates 事件早于装配完成，丢弃")
		return nil
	}
	return h.sink
}

func (h *sinkHolder) OnNew(ctx context.Context, m domain.ChannelMessage) error {
	if s := h.delegate(); s != nil {
		return s.OnNew(ctx, m)
	}
	return nil
}

func (h *sinkHolder) OnEdit(ctx context.Context, m domain.ChannelMessage) error {
	if s := h.delegate(); s != nil {
		return s.OnEdit(ctx, m)
	}
	return nil
}

func (h *sinkHolder) OnDelete(ctx context.Context, ref domain.MessageRef) error {
	if s := h.delegate(); s != nil {
		return s.OnDelete(ctx, ref)
	}
	return nil
}

// messageSink 是 updates 事件的生产消费者：canonical writer（messages 表
// 单一写入协议，02 §2.3）+ 转发引擎事件入口。
type messageSink struct {
	repo   *mysql.MessageRepository
	engine *forwarding.Engine
	lg     *slog.Logger
}

func (s *messageSink) OnNew(ctx context.Context, m domain.ChannelMessage) error {
	if _, err := s.repo.WriteNew(ctx, m); err != nil {
		return fmt.Errorf("canonical 写入: %w", err)
	}
	s.engine.HandleNew(ctx, m)
	return nil
}

func (s *messageSink) OnEdit(ctx context.Context, m domain.ChannelMessage) error {
	// 编辑写修订（幂等）；P0 编辑不触发转发重发。
	return s.repo.WriteEdit(ctx, m)
}

func (s *messageSink) OnDelete(ctx context.Context, ref domain.MessageRef) error {
	return s.repo.WriteDelete(ctx, ref)
}
