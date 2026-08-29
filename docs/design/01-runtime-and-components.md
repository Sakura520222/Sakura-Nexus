# 01 运行时与组件

- 状态：⏳ 待成文
- 受约束 ADR：[001](../decisions/001-telegram-stack.md) · [002](../decisions/002-runtime-model.md) · [003](../decisions/003-webui-form.md) · [005](../decisions/005-go-libraries.md) · [008](../decisions/008-rich-message-transport.md)

## 覆盖内容

- 进程生命周期：启动序列（迁移→配置→客户端→服务→Web）、错误两层与 fatal 退出状态机、graceful shutdown 序列、health endpoint、信号与 systemd 交互
- 代码组织：Go 包结构（`cmd/`、`internal/…`）、前端 `web/`
- 接口边界清单：`Fetcher / Sender / ForwardSender / Retriever / Reranker / AIProvider / VisionProcessor / QueryAnalyzer / MemoryStore`；P1 no-op 策略
- **出站消息抽象（用户指定补充）**：

```go
type Sender interface {
    Send(ctx context.Context, req SendRequest) (SentMessage, error)
}

type MessageRenderer interface {
    Render(ctx context.Context, content MessageContent) (RenderedMessage, error)
}
```

  实现内部路由：普通消息 → MTProtoSender(gotd)；Rich Message → BotAPIRichSender(net/http)。业务层不判断「MTProto 还是 Bot API」。
- 配置体系：`.env` v2 完整清单、MySQL settings scope 划分、加载顺序与热更新流程
