// Package config 负责 .env 引导配置（bootstrap only）与 MySQL settings
// 配置中心（scope→typed struct、快照、热更新回调）。
// 设计：docs/design/01-runtime-and-components.md §6。
package config
