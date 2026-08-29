// Command smoke-recovery 是 T1.3/GATE-1 的最终冒烟：完整 wiring（Manager +
// StateStorage + PeerStorage + dispatcher + canonical writer）下验证
// 真实频道消息可靠落库与重启 catch-up。
//
// 验证流程（GATE-1）：
//
//	go run ./cmd/smoke/smoke-recovery -duration 90s
//	  → 运行期间在任一已加入的频道发几条消息，观察 "✓ NEW" 落库行
//	停止程序，离线期间在频道再发几条
//	go run ./cmd/smoke/smoke-recovery -duration 30s
//	  → 启动后应看到离线期间消息被 getDifference 补齐（NEW 行）
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gotd/td/telegram/updates"

	"github.com/Sakura520222/Sakura-Nexus/internal/config"
	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/mysql"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/telegram"
)

func main() {
	duration := flag.Duration("duration", 60*time.Second, "监听时长")
	flag.Parse()

	env, err := config.Load()
	if err != nil {
		fail("配置加载失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()

	lg := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

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

	var before int
	if err := db.Get(&before, "SELECT COUNT(*) FROM messages"); err != nil {
		fail("统计启动前消息数: %v", err)
	}
	fmt.Printf("启动前 messages 总数: %d\n", before)

	repo := mysql.NewMessageRepository(db)
	sink := &printingSink{repo: repo}

	user, manager := telegram.SetupUserUpdates(int(env.TelegramAPIID), env.TelegramAPIHash,
		mysql.NewSessionStorage(db, "user"),
		telegram.UpdatesConfig{
			State: mysql.NewStateStorage(db, "user"),
			Peers: mysql.NewPeerStorage(db, "user"),
			Sink:  sink,
			Log:   lg,
		})

	err = user.Run(ctx, func(ctx context.Context) error {
		authorized, err := user.Authorized(ctx)
		if err != nil {
			return err
		}
		if !authorized {
			fmt.Println("✗ 尚未登录：请先运行 go run ./cmd/sakura-nexus login-user")
			os.Exit(1)
		}
		self, err := user.Self(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("✓ User: id=%d @%s\n", self.ID, self.Username)
		fmt.Printf("… 监听 %s（期间在频道发消息；离线补齐验证见文件头说明）\n", duration)

		return manager.Run(ctx, user.Raw().API(), self.ID, updates.AuthOptions{
			OnStart: func(ctx context.Context) {
				fmt.Println("✓ updates.Manager 已启动（state 恢复 + difference 完成）")
			},
		})
	})
	if err != nil && ctx.Err() == nil {
		fail("smoke-recovery: %v", err)
	}

	fmt.Printf("结果：本次进程新落库 %d 条（messages 总数 %d → %d）\n",
		sink.newCount.Load(), before, before+int(sink.newCount.Load()))
	fmt.Println("✓ smoke-recovery 结束")
}

// printingSink 把 canonical writer 适配为 MessageSink 并打印落库事件。
type printingSink struct {
	repo     *mysql.MessageRepository
	newCount atomic.Int64
}

func (s *printingSink) OnNew(ctx context.Context, m domain.ChannelMessage) error {
	created, err := s.repo.WriteNew(ctx, m)
	if err != nil {
		return err
	}
	if created {
		s.newCount.Add(1)
		fmt.Printf("✓ NEW %s msg=%d %s\n", m.Ref.Chat, m.Ref.MessageID, excerpt(m.Text))
	}
	return nil
}

func (s *printingSink) OnEdit(ctx context.Context, m domain.ChannelMessage) error {
	if err := s.repo.WriteEdit(ctx, m); err != nil {
		return err
	}
	fmt.Printf("✓ EDIT %s msg=%d %s\n", m.Ref.Chat, m.Ref.MessageID, excerpt(m.Text))
	return nil
}

func (s *printingSink) OnDelete(ctx context.Context, ref domain.MessageRef) error {
	if err := s.repo.WriteDelete(ctx, ref); err != nil {
		return err
	}
	fmt.Printf("✓ DELETE %s msg=%d\n", ref.Chat, ref.MessageID)
	return nil
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

func fail(format string, a ...any) {
	fmt.Printf("✗ "+format+"\n", a...)
	os.Exit(1)
}
