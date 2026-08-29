# 06 部署、安全与可观测性

- 状态：📝 已成文，待用户审
- 受约束 ADR：[002](../decisions/002-runtime-model.md) · [005](../decisions/005-go-libraries.md) · [006](../decisions/006-rag-architecture.md)

## 1. 部署形态

### 1.1 裸机 + systemd（主推）

```ini
# deploy/sakura-bot.service
[Unit]
Description=Sakura-Bot v2
After=network-online.target mysqld.service qdrant.service
Wants=network-online.target

[Service]
User=sakura
ExecStart=/usr/local/bin/sakura-bot
EnvironmentFile=/etc/sakura-bot/.env    # 权限 600，属主 sakura
Restart=on-failure                     # 非 watchdog（01 §1.5）
RestartSec=5
# 资源保护
MemoryHigh=512M
MemoryMax=768M

[Install]
WantedBy=multi-user.target
```

Qdrant 与 MySQL 用系统包/官方二进制各自 systemd 管理（文档给出 qdrant.service 示例）。

### 1.2 Docker（单容器装单二进制，ADR-002）

```dockerfile
# 多阶段：node:22 pnpm build → golang:1.26 build（CGO_ENABLED=0）→ distroless/static 运行
# 非 root 用户；HEALTHCHECK CMD GET /api/health；产物仅一个静态二进制（前端已 embed）
```

### 1.3 docker-compose

```yaml
services:
  sakura-bot:
    build: .
    env_file: .env
    restart: unless-stopped
    mem_limit: 768m
    depends_on: {mysql: {condition: service_healthy}}
  mysql:            # profile: full（默认假设外部 MySQL；--profile full 一键起全套）
    image: mysql:8
    volumes: [mysql_data:/var/lib/mysql]
    healthcheck: {test: mysqladmin ping, interval: 10s, retries: 10}
  qdrant:           # profile: full
    image: qdrant/qdrant
    environment: [QDRANT__SERVICE__API_KEY=…]
    volumes: [qdrant_data:/qdrant/storage]
    # 不映射公网端口
profiles: [full]
```

## 2. 升级与回滚

- 升级：停 → 替换二进制 → 启动（goose 自动前向迁移，01 §1.1）。
- 迁移纪律：**只加不改**；需改列语义时先新增列、双版本窗口后再删旧列；保证回滚一个版本可用。
- WebUI「重启」按钮：优雅退出（exit 0）由 systemd/compose 拉起新进程（ADR-002 语义）。

## 3. 备份与恢复

| 对象 | 策略 |
|---|---|
| MySQL | 每日 `mysqldump`（含全部 SoT：配置/消息/修订/会话/水位/session/update state）+ 定期 binlog 可选 |
| Qdrant | **不备份**（Derived/Disposable，ADR-006）；恢复 = 空 collection + reindex worker 重建 |
| `.env` | 离线备份（含全部 bootstrap 凭据） |
| 临时文件 | 不备份（启动自动清理） |

## 4. 可观测性

- **日志**：slog JSON → stdout（journald/docker logs 聚合）；可选文件轮转（settings.logging）。组件 logger：`forwarding / summary / rag / conversation / user / bot / webapi / db / botapi`。
- **实时日志流**：环形缓冲 512 条（容量内服务端过滤：级别/组件/关键字）→ WebSocket（04 §5）。
- **指标（进程内聚合，不引入独立 metrics 栈——低占用）**：转发成功/失败/FloodWait 次数、发送队列深度、derived-index 队列深度与丢弃计数、repair 补做数、reindex 进度、各客户端可用性状态 → `GET /api/system/status` 暴露；未来需要时可加 text exposition 端点。
- **审计**：全部配置/规则/系统操作写 `system_audit_logs`（actor/action/detail）。
- **通知**：启动/严重错误经 Bot 通知管理员（settings.system.notify 开关）。

## 5. 安全

| 项 | 措施 |
|---|---|
| `.env` | 权限 600、属主运行用户；secrets 边界见 01 §6.4（永不回显） |
| WebUI | JWT 12h；登录失败锁定（04 §4）；默认建议监听 `127.0.0.1` + 反向代理 TLS；公网直接暴露时文档给出最小化建议（防火墙/fail2ban） |
| Qdrant | API key + compose 内网（不映射公网端口）；裸机部署绑定 127.0.0.1 |
| MySQL | 专用最小权限账号（仅 sakura_bot 库）；绑定内网/127.0.0.1 |
| Bot token 日志脱敏 | platform/botapi 的 HTTP 日志**不打印完整 URL**（token 在 path 中）；错误信息脱敏 |
| 依赖漏洞 | CI 跑 `govulncheck`；前端 `pnpm audit` |
| Telegram 侧 | Bot 仅管理员可用管理命令（admins 表）；User session 是最高价值资产——只在 MySQL 与内存中出现，日志/审计永不输出 |

## 6. 资源预算（验证性目标，非承诺——ADR-001 内存预期原则）

- P0 常驻目标：< 150MB RSS（Go 运行时 + 双 MTProto 客户端 + Web 服务）；验证手段见 07 §3（24h 稳定性观察）。
- 连接：MySQL ≤ max_open_conns（默认 5）；Qdrant gRPC 1 连接；Bot API 按需短连接复用。
- 临时文件：即时删除 + 启动清理（03 §3.9）。
- goroutine：数量级 ~10²；supervisor 定期打点（system/status 可见）。
