-- +goose Up
-- 0002 P0 业务表（P0 Plan §4；字段依据冻结设计 02-storage §2.2/§2.8 R3.1.1）。

CREATE TABLE settings (
    scope      VARCHAR(32) NOT NULL COMMENT 'system / forwarding / logging / ai / summary / taxonomy / rag / qa',
    data       JSON        NOT NULL COMMENT '与该 scope 的 Go struct 一一对应（01 §6.2，编译期 schema）',
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (scope)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE channels (
    id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tg_id              BIGINT UNSIGNED NOT NULL COMMENT '频道裸 ID（唯一稳定标识）',
    username           VARCHAR(64)  NULL COMMENT '解析辅助（可变，非身份）',
    title              VARCHAR(255) NULL COMMENT '展示快照',
    discussion_chat_id BIGINT UNSIGNED NULL COMMENT '关联讨论群裸 ID（Telegram 提供）',
    is_verified        TINYINT(1)   NOT NULL DEFAULT 0,
    added_at           DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_channels_tg_id (tg_id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE channel_settings (
    channel_id     BIGINT UNSIGNED NOT NULL COMMENT '→ channels.tg_id',
    summary_config JSON NULL COMMENT '调度（frequency/days/hour/minute、回源频道；P1 struct 校验）',
    poll_config    JSON NULL,
    welcome_config JSON NULL,
    updated_at     DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (channel_id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forward_rules (
    id                   INT UNSIGNED NOT NULL AUTO_INCREMENT,
    name                 VARCHAR(128) NULL,
    source_chat_type     VARCHAR(8)      NOT NULL COMMENT 'user/chat/channel（ChatRef 完整持久化 R3.1.1）',
    source_chat_id       BIGINT UNSIGNED NULL,
    source_username      VARCHAR(64)     NULL,
    target_chat_type     VARCHAR(8)      NOT NULL,
    target_chat_id       BIGINT UNSIGNED NULL,
    target_username      VARCHAR(64)     NULL,
    enabled              TINYINT(1)      NOT NULL DEFAULT 1,
    keywords             JSON NULL,
    blacklist            JSON NULL,
    patterns             JSON NULL COMMENT '正则（struct 校验可编译）',
    blacklist_patterns   JSON NULL,
    media_types          JSON NULL,
    forward_original_only TINYINT(1)     NOT NULL DEFAULT 0,
    copy_mode            VARCHAR(16)     NOT NULL DEFAULT 'copy' COMMENT 'copy / forward（需 Bot 可读源）',
    ai_enabled           TINYINT(1)      NOT NULL DEFAULT 0,
    ai_prompt            TEXT NULL,
    custom_footer        TEXT NULL,
    delay_min_sec        FLOAT NOT NULL DEFAULT 0.5,
    delay_max_sec        FLOAT NOT NULL DEFAULT 2.0,
    last_message_id      BIGINT UNSIGNED NULL COMMENT 'contiguous cursor（P0 Plan §6）',
    created_at           DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at           DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    INDEX idx_rules_source (source_chat_type, source_chat_id),
    INDEX idx_rules_source_username (source_username)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forwarded_messages (
    source_chat_type    VARCHAR(8)      NOT NULL,
    source_chat_id      BIGINT UNSIGNED NOT NULL,
    source_message_id   BIGINT UNSIGNED NOT NULL,
    target_chat_type    VARCHAR(8)      NOT NULL,
    target_chat_id      BIGINT UNSIGNED NOT NULL,
    rule_id             INT UNSIGNED    NULL,
    target_message_id   BIGINT UNSIGNED NULL COMMENT '目标消息映射（未来编辑/删除同步钩子）',
    content_hash        CHAR(64)        NULL,
    created_at          DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (source_chat_type, source_chat_id, source_message_id, target_chat_type, target_chat_id),
    INDEX idx_fwd_hash (content_hash),
    INDEX idx_fwd_created (created_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE forwarding_stats (
    rule_id         INT UNSIGNED NOT NULL,
    stat_date       DATE         NOT NULL,
    forwarded_count INT UNSIGNED NOT NULL DEFAULT 0,
    failed_count    INT UNSIGNED NOT NULL DEFAULT 0,
    PRIMARY KEY (rule_id, stat_date)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE system_audit_logs (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    actor      VARCHAR(128) NULL COMMENT 'webui:<username> / tg:<user_id> / system',
    action     VARCHAR(64)  NOT NULL,
    detail     JSON         NULL,
    created_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    INDEX idx_audit_created (created_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- +goose Down
DROP TABLE IF EXISTS system_audit_logs;
DROP TABLE IF EXISTS forwarding_stats;
DROP TABLE IF EXISTS forwarded_messages;
DROP TABLE IF EXISTS forward_rules;
DROP TABLE IF EXISTS channel_settings;
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS settings;
