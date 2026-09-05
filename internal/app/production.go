package app

// Production 装配（T5.1 接线收口；01 §1.1 启动序列 2–8）：MySQL → settings →
// platform 构造 → 领域 service 构造（不启动）→ 注册 lifecycle。
// gotd/sqlx/openai-go 全部封在 platform 层，本文件只做组合（01 §2.2）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sakura520222/Sakura-Nexus/internal/config"
	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
	"github.com/Sakura520222/Sakura-Nexus/internal/forwarding"
	"github.com/Sakura520222/Sakura-Nexus/internal/logging"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/ai"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/botapi"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/mysql"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/telegram"
	"github.com/Sakura520222/Sakura-Nexus/internal/webapi"
)

// Production 是装配产物：lifecycle App + T5.3 WebAPI 需要的句柄。
type Production struct {
	App *App
	Log *slog.Logger // 根 logger（LevelVar 可动态调整 + 环形缓冲供 WS 流）

	Engine   *forwarding.Engine
	Settings *config.SettingsCenter
	Ring     *logging.Ring // 日志环形缓冲（WS /api/ws 日志流快照+实时）

	// RequestRestart 供 WebUI restart（exit 75 全链，01 §1.4；T5.3 system API 消费）。
	RequestRestart func()
	// SetLogLevel 动态调整根 logger 级别（PUT /api/system/log-level）。
	SetLogLevel func(level string) error
	// Close 释放装配期资源（环形缓冲订阅流等）。
	Close func()
}

// Assemble 按冻结启动序列构造全部组件并注册（不启动；App.Run 统一启动）。
// MySQL 连接/迁移失败返回错误（CORE，main 以 exit 1 终止）。
func Assemble(ctx context.Context, env *config.Env) (*Production, error) {
	// 1. slog setup（01 §1.1 步骤 1）：文本主输出 + 环形缓冲（WS 日志流），
	// 级别经 LevelVar 可动态调整。
	levelVar := new(slog.LevelVar)
	levelVar.Set(parseLevelEnv(env.LogLevel))
	ring := logging.NewRing(1024)
	lg := slog.New(logging.NewFanout(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: levelVar}),
		ring,
	))

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
		webapi.WithLogRing(ring),
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

	// userbot 向导（04 §2；独立连接跑登录流，状态经主 user 客户端查询）。
	wizard := telegram.NewWizardService(int(env.TelegramAPIID), env.TelegramAPIHash,
		mysql.NewSessionStorage(db, "user"),
		func(ctx context.Context) (bool, string, int64, error) {
			authed, err := user.Authorized(ctx)
			if err != nil {
				return false, "", 0, err
			}
			username, id := "", int64(0)
			if authed {
				if self, err := user.Self(ctx); err == nil {
					username, id = self.Username, self.ID
				}
			}
			return authed, username, id, nil
		}, lg)
	userbot := &userbotControl{wizard: wizard, user: user}

	// WebAPI 业务依赖（04 §2；webapi 侧只见消费方最小接口）。
	web.ApplyDeps(webapi.Deps{
		Settings:       settingsAdapter{center},
		Engine:         engine,
		Rules:          ruleRepo,
		Channels:       channelRepo,
		Stats:          dedupRepo,
		Userbot:        userbot,
		RequestRestart: a.RequestRestart,
		SetLogLevel: func(level string) error {
			l, err := parseLevelStrict(level)
			if err != nil {
				return err
			}
			levelVar.Set(l)
			return nil
		},
		Audit: mysql.NewAuditRepo(db),
	})

	return &Production{
		App:            a,
		Log:            lg,
		Engine:         engine,
		Settings:       center,
		Ring:           ring,
		RequestRestart: a.RequestRestart,
		SetLogLevel: func(level string) error {
			l, err := parseLevelStrict(level)
			if err != nil {
				return err
			}
			levelVar.Set(l)
			return nil
		},
		Close: ring.Close,
	}, nil
}

// parseLevelEnv 解析 .env LOG_LEVEL（非法/空回落 info）。
func parseLevelEnv(s string) slog.Level {
	l, err := parseLevelStrict(s)
	if err != nil {
		return slog.LevelInfo
	}
	return l
}

// parseLevelStrict 严格解析（PUT log-level 校验用）。
func parseLevelStrict(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("非法日志级别: %q（debug|info|warn|error）", s)
	}
}

// userbotControl 组合 WizardService（向导三步）与主 user 客户端
// （status/logout/join），满足 webapi.UserbotControl。
type userbotControl struct {
	wizard *telegram.WizardService
	user   *telegram.UserClient
}

func (c *userbotControl) Status(ctx context.Context) (bool, string, int64, error) {
	return c.wizard.Status(ctx)
}

func (c *userbotControl) LoginStart(ctx context.Context, phone string) (string, error) {
	return c.wizard.LoginStart(ctx, phone)
}

func (c *userbotControl) LoginCode(ctx context.Context, requestID, code string) (bool, error) {
	return c.wizard.LoginCode(ctx, requestID, code)
}

func (c *userbotControl) LoginPassword(ctx context.Context, requestID, password string) error {
	return c.wizard.LoginPassword(ctx, requestID, password)
}

func (c *userbotControl) Logout(ctx context.Context) error { return c.user.Logout(ctx) }

func (c *userbotControl) Join(ctx context.Context, chat string) error {
	return c.user.JoinChannel(ctx, chat)
}

// settingsAdapter 把 *config.SettingsCenter 适配为 webapi.SettingsControl
// （快照转 JSON 兼容 map；secret 脱敏在 webapi 侧按 scope 执行）。
type settingsAdapter struct{ c *config.SettingsCenter }

func (a settingsAdapter) Snapshot(scope string) (map[string]any, error) {
	var v any
	switch scope {
	case "system":
		v = a.c.System()
	case "forwarding":
		v = a.c.Forwarding()
	case "logging":
		v = a.c.Logging()
	case "ai":
		v = a.c.AI()
	default:
		return nil, fmt.Errorf("未知 scope: %s", scope)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (a settingsAdapter) Update(ctx context.Context, scope string, partial map[string]any) error {
	return a.c.Update(ctx, scope, partial)
}

func (a settingsAdapter) Scopes() []string {
	return []string{"system", "forwarding", "logging", "ai"}
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
