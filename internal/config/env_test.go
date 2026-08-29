package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setFullEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MYSQL_HOST", "db.local")
	t.Setenv("MYSQL_PORT", "3307")
	t.Setenv("MYSQL_USER", "u")
	t.Setenv("MYSQL_PASSWORD", "p")
	t.Setenv("MYSQL_DATABASE", "d")
	t.Setenv("MYSQL_MAX_OPEN_CONNS", "9")
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:abc")
	t.Setenv("TELEGRAM_API_ID", "12345")
	t.Setenv("TELEGRAM_API_HASH", "hash")
	t.Setenv("WEBUI_HOST", "0.0.0.0")
	t.Setenv("WEBUI_PORT", "9999")
	t.Setenv("WEBUI_USERNAME", "admin2")
	t.Setenv("WEBUI_PASSWORD", "pw")
}

func TestLoadFullEnv(t *testing.T) {
	setFullEnv(t)
	env, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if env.MySQLHost != "db.local" || env.MySQLPort != 3307 || env.MySQLMaxOpenConns != 9 {
		t.Errorf("MySQL 字段不符: %+v", env)
	}
	if env.TelegramAPIID != 12345 || env.TelegramBotToken != "123:abc" {
		t.Errorf("Telegram 字段不符: %+v", env)
	}
	if env.WebUIPort != 9999 || env.WebUIHost != "0.0.0.0" {
		t.Errorf("WebUI 字段不符: %+v", env)
	}
}

func TestLoadDefaults(t *testing.T) {
	// 仅设必填，其余取默认值
	t.Setenv("MYSQL_PASSWORD", "p")
	t.Setenv("TELEGRAM_BOT_TOKEN", "1:a")
	t.Setenv("TELEGRAM_API_ID", "10")
	t.Setenv("TELEGRAM_API_HASH", "h")
	t.Setenv("WEBUI_PASSWORD", "pw")

	env, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if env.MySQLHost != "localhost" || env.MySQLPort != 3306 || env.MySQLDatabase != "sakura_bot" {
		t.Errorf("MySQL 默认值不符: %+v", env)
	}
	if env.WebUIHost != "127.0.0.1" || env.WebUIPort != 8080 || env.WebUIUsername != "admin" {
		t.Errorf("WebUI 默认值不符（WEBUI_HOST 裸机默认 127.0.0.1）: %+v", env)
	}
	if env.LogLevel != "info" || env.ShutdownTimeoutSeconds != 30 {
		t.Errorf("可选默认值不符: %+v", env)
	}
	if env.QdrantURL != "" || env.QdrantAPIKey != "" {
		t.Errorf("Qdrant P0 应允许为空: %+v", env)
	}
}

func TestMissingReportsAllRequired(t *testing.T) {
	// 全部必填缺失 → 一次性报全量（有序）
	_, err := Load()
	var me *MissingEnvError
	if !errors.As(err, &me) {
		t.Fatalf("期望 MissingEnvError，得到 %v", err)
	}
	want := []string{
		"MYSQL_PASSWORD", "TELEGRAM_API_HASH", "TELEGRAM_API_ID",
		"TELEGRAM_BOT_TOKEN", "WEBUI_PASSWORD",
	}
	if len(me.Missing) != len(want) {
		t.Fatalf("缺失项数量 %d != %d: %v", len(me.Missing), len(want), me.Missing)
	}
	for i := range want {
		if me.Missing[i] != want[i] {
			t.Errorf("缺失项[%d] %s != %s", i, me.Missing[i], want[i])
		}
	}
}

func TestMissingSingle(t *testing.T) {
	for _, key := range []string{
		"TELEGRAM_BOT_TOKEN", "TELEGRAM_API_HASH", "WEBUI_PASSWORD", "MYSQL_PASSWORD",
	} {
		t.Run(key, func(t *testing.T) {
			setFullEnv(t)
			t.Setenv(key, "") // 置空与未设置等价（必填判空、数值走默认）
			_, err := Load()
			var me *MissingEnvError
			if !errors.As(err, &me) {
				t.Fatalf("期望 MissingEnvError，得到 %v", err)
			}
			if len(me.Missing) != 1 || me.Missing[0] != key {
				t.Errorf("缺失项应为 [%s]，得到 %v", key, me.Missing)
			}
		})
	}
}

func TestAPIIDZeroIsMissing(t *testing.T) {
	setFullEnv(t)
	t.Setenv("TELEGRAM_API_ID", "0") // 0 视为未配置
	_, err := Load()
	var me *MissingEnvError
	if !errors.As(err, &me) {
		t.Fatalf("期望 MissingEnvError，得到 %v", err)
	}
	if me.Missing[0] != "TELEGRAM_API_ID" {
		t.Errorf("应报 TELEGRAM_API_ID，得到 %v", me.Missing)
	}
}

func TestInvalidNumeric(t *testing.T) {
	cases := map[string]string{
		"MYSQL_PORT":        "abc",
		"TELEGRAM_API_ID":   "not-a-number",
		"WEBUI_PORT":        "",
	}
	// WEBUI_PORT 空串走默认，不非法——单独覆盖
	setFullEnv(t)
	t.Setenv("MYSQL_PORT", cases["MYSQL_PORT"])
	if _, err := Load(); err == nil {
		t.Error("MYSQL_PORT=abc 应报错")
	} else if !strings.Contains(err.Error(), "MYSQL_PORT") {
		t.Errorf("错误应含变量名 MYSQL_PORT: %s", err)
	}

	setFullEnv(t)
	t.Setenv("TELEGRAM_API_ID", cases["TELEGRAM_API_ID"])
	if _, err := Load(); err == nil {
		t.Error("TELEGRAM_API_ID=not-a-number 应报错")
	}
}

func TestDotenvFileLoaded(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".env")
	content := "MYSQL_PASSWORD=filepw\nTELEGRAM_BOT_TOKEN=filetok\n" +
		"TELEGRAM_API_ID=777\nTELEGRAM_API_HASH=filehash\nWEBUI_PASSWORD=fileweb\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := Load(file)
	if err != nil {
		t.Fatalf("Load(file): %v", err)
	}
	if env.TelegramAPIID != 777 || env.MySQLPassword != "filepw" {
		t.Errorf(".env 文件加载不符: %+v", env)
	}
}
