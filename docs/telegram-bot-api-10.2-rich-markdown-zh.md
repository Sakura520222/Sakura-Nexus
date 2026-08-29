# Telegram Bot API 10.2：Rich Markdown 完整格式支持

> **来源**：Telegram Bot API 官方文档 · **对应版本**：10.2
> **用途**：项目内部中文参考快照（ADR-008 与 Rich renderer golden tests 的规格依据），**非规范性来源**
> **规则冲突时**：以 Telegram 官方最新文档 + ADR-008 为准

> 抓取时间：2026-07-17（Asia/Shanghai）  
> 官方更新：Bot API 10.1 于 2026-06-11 引入 Rich Messages；10.2 于 2026-07-14 补充媒体声明、语音消息与输入块。  
> 官方文档：[Rich Message Formatting Options](https://core.telegram.org/bots/api#rich-message-formatting-options) · [Bot Features](https://core.telegram.org/bots/features#rich-messages)

## 1. 这不是 MarkdownV2

Rich Markdown 是新的结构化消息格式，面向报告、AI 流式回复、文档和技术内容。它尽可能兼容 GitHub Flavored Markdown（GFM），并允许在同一段内容中混写受支持的 HTML。

发送时调用 `sendRichMessage`，将内容放入 `rich_message.markdown` 字段；**不要**使用旧消息 API 的 `parse_mode=MarkdownV2`。

Rich Message 还可改用 `rich_message.html` 或结构化的 `rich_message.blocks`，三者必须且只能提供一个。

## 2. 行内格式

~~~~markdown
**粗体**
__粗体（等价写法）__
*斜体*
_斜体（等价写法）_
~~删除线~~
`行内代码`
==高亮==
||剧透||

[普通链接](https://t.me/)
[邮件链接](mailto:user@example.com)
[电话链接](tel:+123456789)
[按 ID 提及用户](tg://user?id=123456789)
![](tg://emoji?id=5368324170671202286)
![明天 22:45](tg://time?unix=1647531900&format=wDT)
$x^2 + y^2$
~~~~

说明：

- `tg://emoji?id=...` 为自定义表情；`tg://time` 为动态日期时间实体。
- 时间 `format` 采用现有的日期时间实体格式，例如 `wDT`、`t`。
- 裸 URL、邮箱、`@用户名`、`#话题`、`$cashtag`、`/命令`、电话和银行卡号会自动识别。传 `skip_entity_detection: true` 可以关闭自动识别。
- 富消息允许深度嵌套（上限见后文）。HTML 行内标签中的 Markdown 也会被解析。

## 3. 标题、段落、代码与分隔线

~~~~markdown
# 一级标题
## 二级标题
### 三级标题
#### 四级标题
##### 五级标题
###### 六级标题

普通段落。段落之间保留空行。

```python
print('带语言标记的代码块')
```

---
~~~~

## 4. 列表、任务清单与引用

~~~~markdown
- 无序列表
* 也可使用星号
+ 也可使用加号

1. 有序列表第一项
2. 有序列表第二项

- [ ] 未完成任务
- [x] 已完成任务

> 引用第一行
>
> 引用下一段
> 最后一行
~~~~

## 5. 媒体块

媒体必须单独占一个块，接受 HTTP/HTTPS URL；类型由 MIME 类型及 URL 推断。URL 后的可选标题会成为媒体说明。

~~~~markdown
![](https://example.com/photo.jpg)
![](https://example.com/video.mp4)
![](https://example.com/audio.mp3)
![](https://example.com/voice.ogg)
![](https://example.com/animation.gif)

![](https://example.com/photo.jpg "图片说明")
![](https://example.com/video.mp4 "视频说明")
~~~~

若要在 Markdown/HTML 中引用本请求上传的文件，使用下列链接，并在 `rich_message.media` 中提供对应 ID 的 `InputRichMessageMedia`：

~~~~markdown
![](tg://photo?id=cover)
![](tg://video?id=intro)
![](tg://audio?id=podcast)
~~~~

`InputRichMessageMedia.id` 长度为 1–64，仅允许 `A-Z`、`a-z`、`0-9`、`_`、`-`；其 `media` 可为 `InputMediaAnimation`、`InputMediaAudio`、`InputMediaPhoto`、`InputMediaVideo` 或 `InputMediaVoiceNote`。

## 6. 表格、脚注与公式

~~~~markdown
| 左对齐 | 居中 | 右对齐 |
|:-------|:----:|-------:|
| 内容   | 内容 | 内容   |

引用脚注[^note]。

[^note]: 脚注定义，可以有 *行内格式*。

$$E = mc^2$$

```math
\int_a^b f(x)\,dx
```
~~~~

- 表格单元格仅允许行内格式。
- 公式源代码按原始 LaTeX 处理。

## 7. 需使用 HTML 的富格式

Rich Markdown 可混入任意 HTML，但只有官方列出的富消息标签会被解析。以下功能没有 Markdown 原生语法，应改用 HTML。

```html
<u>下划线</u> <ins>下划线</ins>
<sub>下标</sub> <sup>上标</sup>

<a name="chapter-1"></a>
<a href="#chapter-1">文内锚点链接</a>

<aside>拉引文<cite>作者</cite></aside>
<details open><summary>默认展开的标题</summary>可折叠内容</details>

<tg-map lat="41.9" long="12.5" zoom="14"/>
<tg-collage><img src="https://example.com/a.jpg"/><video src="https://example.com/b.mp4"/></tg-collage>
<tg-slideshow><img src="https://example.com/a.jpg"/><video src="https://example.com/b.mp4"/></tg-slideshow>
```

以下 HTML 标签也受支持：

- 文本：`<b>/<strong>`、`<i>/<em>`、`<u>/<ins>`、`<s>/<strike>/<del>`、`<code>`、`<mark>`、`<sub>`、`<sup>`、`<tg-spoiler>`。
- 链接/引用：`<a href>`、`<a name>`、`<tg-reference name>`、`<tg-emoji emoji-id>`、`<tg-time>`、`<tg-math>`。
- 块：`<h1>` 至 `<h6>`、`<p>`、`<pre>`、`<pre><code class="language-...">`、`<footer>`、`<hr>`、`<ul>`、`<ol>`、`<li>`、`<input type="checkbox">`、`<blockquote>`、`<aside>`。
- 媒体与组合：`<img>`、`<video>`、`<audio>`、`<figure>`、`<figcaption>`、`<cite>`、`<tg-map>`、`<tg-collage>`、`<tg-slideshow>`。
- 结构：`<table>`、`<caption>`、`<tr>`、`<th>`、`<td>`、`<details>`、`<summary>`、`<tg-math-block>`。

注意：除 `<details>`、`<tg-collage>`、`<tg-slideshow>` 外，Markdown 不会在块级 HTML 标签内部解析；这些位置应改用 HTML 标签。

## 8. 流式 AI 回复专用占位块

`sendRichMessageDraft` 可以额外使用：

```html
<tg-thinking>正在思考…</tg-thinking>
```

草稿仅供目标私聊预览，30 秒后失效；同一 `draft_id` 的更新会带动画。生成完成后，必须再调用 `sendRichMessage` 发送完整消息，才能永久保留。草稿 API 不支持直接上传新文件。

## 9. 限制

- 富消息文本最多 32,768 个 UTF-8 字符（包括自定义表情替代文本和公式源）。
- 最多 500 个块，嵌套块、列表项、表格行、引用块和 details 块均计入。
- 格式与块的嵌套深度最多 16 层。
- 最多 50 个媒体附件（图片、视频、音频合计）。
- 表格最多 20 列。

## 10. 最小请求示例

```json
{
  "chat_id": 123456789,
  "rich_message": {
    "markdown": "## 部署报告\n\n- [x] 已完成\n- [ ] 待验证\n\n状态：**正常**，耗时 $2.4\\,s$。",
    "skip_entity_detection": false
  }
}
```

将上面的 JSON 作为 `sendRichMessage` 的请求体发送。常用附加参数包括 `message_thread_id`、`disable_notification`、`protect_content`、`reply_parameters` 与 `reply_markup`。

## 11. 与普通消息的选择

| 场景 | 推荐 |
|---|---|
| 短通知、简单聊天流程 | `sendMessage` + `parse_mode=MarkdownV2` |
| 标题、任务列表、表格、公式、脚注、媒体、折叠内容、AI 流式输出 | `sendRichMessage` + `rich_message.markdown` |

## 官方来源

- [Bot API：Rich Message Formatting Options](https://core.telegram.org/bots/api#rich-message-formatting-options)
- [Bot API：InputRichMessage](https://core.telegram.org/bots/api#inputrichmessage)
- [Bot API：sendRichMessage](https://core.telegram.org/bots/api#sendrichmessage)
- [Bot API：sendRichMessageDraft](https://core.telegram.org/bots/api#sendrichmessagedraft)
- [Bot Features：Rich Messages](https://core.telegram.org/bots/features#rich-messages)
- [Bot API Changelog](https://core.telegram.org/bots/api-changelog)
