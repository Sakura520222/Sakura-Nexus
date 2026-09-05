// Command sakura-nexus 是 Sakura-Nexus 的唯一入口（composition root，01 §2.1）：
// .env → app.Assemble → App.Run。无业务逻辑。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/Sakura520222/Sakura-Nexus/internal/app"
	"github.com/Sakura520222/Sakura-Nexus/internal/config"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login-user":
			runLoginUser(os.Args[2:])
			return
		}
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionString())
		return
	}
	os.Exit(runApp())
}

// runApp 生产启动编排（01 §1.1 启动序列；退出码 01 §1.4）：
// 0 正常 / 1 CORE fatal（装配失败等）/ 2 配置缺失 / 75 重启请求。
func runApp() int {
	env, err := config.Load()
	if err != nil {
		fmt.Printf("✗ %v\n", err)
		return 2 // 配置缺失（MissingEnvError 及加载失败同码）
	}
	lg := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(env.LogLevel),
	}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	prod, err := app.Assemble(ctx, env, lg)
	if err != nil {
		lg.Error("装配失败（CORE fatal）", "err", err)
		return 1
	}

	code := prod.App.Run(ctx)
	lg.Info("进程退出", "code", code)
	return code
}

// parseLevel 解析 LOG_LEVEL（非法值回落 info，不静默吞配置错误之外的场景）。
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func versionString() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "devel"
}

// fail 打印错误并以退出码 1 结束（login-user 等子命令共用）。
func fail(format string, a ...any) {
	fmt.Printf("✗ "+format+"\n", a...)
	os.Exit(1)
}
