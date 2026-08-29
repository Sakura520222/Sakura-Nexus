// Package migrations 承载全部 goose 迁移并经 embed 嵌入二进制（01 §1.1 启动即迁移）。
package migrations

import "embed"

// FS 嵌入全部迁移 SQL；goose provider 以此为迁移源。
//
//go:embed *.sql
var FS embed.FS
