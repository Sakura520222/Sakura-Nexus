// Package config 负责 .env 引导配置（bootstrap only）与 MySQL settings
// 配置中心（scope→typed struct、快照、热更新回调）。
// 设计：docs/design/01-runtime-and-components.md §6。
package config

import (
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Env 是 .env bootstrap 配置（01 §6.1）：仅引导凭据与连接信息；
// 全部业务配置位于 MySQL settings 配置中心（P0 T2.3）。
type Env struct {
	MySQLHost         string
	MySQLPort         int
	MySQLUser         string
	MySQLPassword     string
	MySQLDatabase     string
	MySQLMaxOpenConns int

	TelegramBotToken string
	TelegramAPIID    int64
	TelegramAPIHash  string

	UserbotPhoneNumber string

	QdrantURL    string
	QdrantAPIKey string

	WebUIHost     string
	WebUIPort     int
	WebUIUsername string
	WebUIPassword string

	LogLevel               string
	ShutdownTimeoutSeconds int

	// MediaTmpDir 是媒体临时文件根目录（可选，MEDIA_TMP_DIR；03 §3.9「目录可配」；
	// 空 = 系统临时目录下 sakura-nexus/ 子目录）。
	MediaTmpDir string
}

// MissingEnvError 报告全部缺失的必填项（一次性全量列出，便于修复）。
type MissingEnvError struct {
	Missing []string
}

func (e *MissingEnvError) Error() string {
	return "缺少必填 .env 配置: " + strings.Join(e.Missing, ", ")
}

// requiredEnv 为必填项（01 §6.1「必需」：Telegram 凭据、WebUI 密码、MySQL 密码）。
// Qdrant 两项 P0 可空（P1 起才需要）。
var requiredEnv = map[string]func(*Env) bool{
	"TELEGRAM_BOT_TOKEN": func(e *Env) bool { return e.TelegramBotToken != "" },
	"TELEGRAM_API_ID":    func(e *Env) bool { return e.TelegramAPIID > 0 },
	"TELEGRAM_API_HASH":  func(e *Env) bool { return e.TelegramAPIHash != "" },
	"WEBUI_PASSWORD":     func(e *Env) bool { return e.WebUIPassword != "" },
	"MYSQL_PASSWORD":     func(e *Env) bool { return e.MySQLPassword != "" },
}

// Load 读取 .env（可选；不覆盖已存在的进程环境变量）并构造 Env。
// 必填缺失时返回 *MissingEnvError（含全部缺失项）；数值字段非法时报含变量名的错误。
func Load(files ...string) (*Env, error) {
	if len(files) > 0 {
		_ = godotenv.Overload(files...)
	} else {
		_ = godotenv.Load() // 默认 .env；不存在则忽略（凭据可全部来自进程环境）
	}

	env := &Env{
		MySQLHost:          getenv("MYSQL_HOST", "localhost"),
		MySQLUser:          getenv("MYSQL_USER", "sakura"),
		MySQLPassword:      os.Getenv("MYSQL_PASSWORD"),
		MySQLDatabase:      getenv("MYSQL_DATABASE", "sakura_bot"),
		TelegramBotToken:   os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramAPIHash:    os.Getenv("TELEGRAM_API_HASH"),
		UserbotPhoneNumber: os.Getenv("USERBOT_PHONE_NUMBER"),
		QdrantURL:          os.Getenv("QDRANT_URL"),
		QdrantAPIKey:       os.Getenv("QDRANT_API_KEY"),
		WebUIHost:          getenv("WEBUI_HOST", "127.0.0.1"),
		WebUIUsername:      getenv("WEBUI_USERNAME", "admin"),
		WebUIPassword:      os.Getenv("WEBUI_PASSWORD"),
		LogLevel:           getenv("LOG_LEVEL", "info"),
		MediaTmpDir:        os.Getenv("MEDIA_TMP_DIR"), // 可选；空 = 系统临时目录下 sakura-nexus/（03 §3.9）
	}

	var err error
	if env.MySQLPort, err = getint("MYSQL_PORT", 3306); err != nil {
		return nil, err
	}
	if env.MySQLMaxOpenConns, err = getint("MYSQL_MAX_OPEN_CONNS", 5); err != nil {
		return nil, err
	}
	if env.TelegramAPIID, err = getint64("TELEGRAM_API_ID", 0); err != nil {
		return nil, err
	}
	if env.WebUIPort, err = getint("WEBUI_PORT", 8080); err != nil {
		return nil, err
	}
	if env.ShutdownTimeoutSeconds, err = getint("SHUTDOWN_TIMEOUT_SECONDS", 30); err != nil {
		return nil, err
	}

	var missing []string
	for name, satisfied := range requiredEnv {
		if !satisfied(env) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, &MissingEnvError{Missing: missing}
	}
	return env, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getint(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, &InvalidEnvError{Key: key, Value: v, Err: err}
	}
	return n, nil
}

func getint64(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, &InvalidEnvError{Key: key, Value: v, Err: err}
	}
	return n, nil
}

// InvalidEnvError 表示数值型 .env 变量格式非法。
type InvalidEnvError struct {
	Key   string
	Value string
	Err   error
}

func (e *InvalidEnvError) Error() string {
	return "配置项 " + e.Key + "=" + e.Value + " 不是合法数字: " + e.Err.Error()
}
