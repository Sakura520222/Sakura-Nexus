# 06 部署、安全与可观测性

- 状态：📝 R3.1.1，待用户核对修改点
- 受约束 ADR：[002](../decisions/002-runtime-model.md) · [005](../decisions/005-go-libraries.md) · [006](../decisions/006-rag-architecture.md)

## 1. 部署形态

### 1.1 裸机 + systemd（主推）

```ini
# deploy/sakura-bot.service（通用 unit：不硬绑具体 MySQL/Qdrant 服务名——各发行版命名不一，P0 也不依赖 Qdrant）
[Unit]
Wants=network-online.target
After=network-online.target

[Service]
User=sakura
ExecStart=/usr/local/bin/sakura-bot
EnvironmentFile=/etc/sakura-bot/.env    # 权限 600，属主 sakura
Restart=on-failure                     # 非 watchdog（01 §1.5）；exit 75（重启请求）非零同样会拉起
RestartSec=5
# 资源保护（示例值，按部署实际调整，非架构硬限制）
MemoryHigh=512M
MemoryMax=768M

[Install]
WantedBy=multi-user.target
```

本机全套部署需要强启动顺序时，提供 optional drop-in（`systemctl edit` 追加 `After=mariadb.service qdrant.service` 之类），不改主 unit。Qdrant 用官方二进制各自 systemd 管理（文档给出 qdrant.service 示例）。

### 1.2 Docker（单容器装单二进制，ADR-002）

```dockerfile
# 多阶段：node:22 pnpm build → golang:1.26 build（CGO_ENABLED=0）→ distroless/static 运行
# 非 root 用户；产物仅一个静态二进制（前端已 embed）
```

- **HEALTHCHECK（R3.1 修正）**：distroless 无 shell/curl/wget，也没有名为 `GET` 的可执行文件——使用程序自带子命令，内部以 `net/http` GET 本机 health endpoint：

```dockerfile
HEALTHCHECK CMD ["/app/sakura-bot", "healthcheck"]
```

### 1.3 docker-compose（R3.1：双文件叠加，不用 profiles）

```text
compose.yaml        # 仅 sakura-bot（连接用户自备的外部 MySQL/Qdrant）
compose.full.yaml   # overlay：+ mysql + qdrant + depends_on 健康依赖
docker compose -f compose.yaml -f compose.full.yaml up -d
```

- profiles 是 service 级属性且未启用的依赖可能形成无效模型，弃用；双文件叠加最直观。
- `sakura-bot` 服务：`build: .`、`env_file: .env`、`restart: unless-stopped`、`mem_limit: 768m`（示例值可调）；compose.full.yaml 中 mysql:8（volume + healthcheck）、qdrant/qdrant（volume + `QDRANT__SERVICE__API_KEY`，**不映射公网端口**）。
- WebUI 监听：compose override 将 `WEBUI_HOST` 设为 `0.0.0.0`（.env 裸机默认 127.0.0.1，01 §6.1）。

## 2. 升级与回滚

- 升级：停 → 替换二进制 → 启动（goose 自动前向迁移，01 §1.1）。
- 迁移纪律：**只加不改**；需改列语义时先新增列、双版本窗口后再删旧列；保证回滚一个版本可用。
- WebUI「重启」按钮：优雅退出后**以退出码 75 结束**（01 §1.4）——systemd `Restart=on-failure` 对非零退出码拉起新进程（exit 0 会被视为 clean exit 而不重启）；docker `unless-stopped` 同样重新拉起。

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
| WebUI | HttpOnly opaque session 12h + SameSite=Strict + Origin 校验 + 登录失败锁定（04 §4）；默认监听 `127.0.0.1` + 反向代理 TLS；公网直接暴露时文档给出最小化建议（防火墙/fail2ban） |
| Qdrant | API key + compose 内网（不映射公网端口）；裸机部署绑定 127.0.0.1 |
| MySQL | 专用最小权限账号（仅 sakura_bot 库）；绑定内网/127.0.0.1 |
| Bot token 日志脱敏 | platform/botapi 的 HTTP 日志**不打印完整 URL**（token 在 path 中）；错误信息脱敏 |
| 依赖漏洞 | CI 跑 `govulncheck`；前端 `pnpm audit` |
| Telegram 侧 | Bot 管理命令白名单 = `settings.system.telegram_admin_ids`（typed config，WebUI 管理；**空 = Telegram 管理命令全部禁用**，用户聊天/问答不受影响——不设独立 admins 表）；User session 是最高价值资产——只在 MySQL 与内存中出现，日志/审计永不输出 |

## 6. 资源预算（验证性目标，非承诺——ADR-001 内存预期原则）

- P0 常驻目标：< 150MB RSS（Go 运行时 + 双 MTProto 客户端 + Web 服务）；验证手段见 07 §3（24h 稳定性观察）。
- 连接：MySQL ≤ max_open_conns（默认 5）；Qdrant gRPC 1 连接；Bot API 按需短连接复用。
- 临时文件：即时删除 + 启动清理（03 §3.9）。
- goroutine：数量级 ~10²；supervisor 定期打点（system/status 可见）。
