//go:build integration

package mysql

import (
	"os"
	"testing"
)

// TestIntegrationEnvContract 是集成测试环境约定骨架（T0.2）：
// 未配置 SAKURA_TEST_MYSQL_* 时跳过；T1.1 起在同一约定上建立真实连接测试。
// 本地环境：自备 MySQL 实例（凭据经 SAKURA_TEST_MYSQL_* 环境变量提供）；
// CI：GitHub Actions service container（.github/workflows/ci.yml）。
func TestIntegrationEnvContract(t *testing.T) {
	if os.Getenv("SAKURA_TEST_MYSQL_HOST") == "" {
		t.Skip("SAKURA_TEST_MYSQL_HOST 未设置（本地：export .env.test.local 中的变量）")
	}
	for _, key := range []string{
		"SAKURA_TEST_MYSQL_PORT", "SAKURA_TEST_MYSQL_USER",
		"SAKURA_TEST_MYSQL_PASSWORD", "SAKURA_TEST_MYSQL_DATABASE",
	} {
		if os.Getenv(key) == "" {
			t.Fatalf("%s 未设置：集成测试环境变量不完整", key)
		}
	}
}
