// Package platform 是基础设施具体实现层——全项目唯一允许 import
// gotd/td、sqlx、openai-go、qdrant client 与 net/http（限 botapi）的层。
// 设计：docs/design/01-runtime-and-components.md §2（depguard 强制）。
package platform
