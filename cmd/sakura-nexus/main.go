// Command sakura-nexus 是 Sakura-Nexus 的唯一入口（composition root）。
//
// 当前仅提供版本信息；完整启动编排（lifecycle/supervisor）在 P0 T2.0 接入，
// 详见 docs/plans/p0-implementation.md 与 docs/design/01-runtime-and-components.md。
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
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
	fmt.Println("sakura-nexus: 启动编排尚未接线（P0 T2.0 接入 lifecycle）")
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
