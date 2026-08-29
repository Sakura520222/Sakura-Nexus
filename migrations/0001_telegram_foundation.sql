-- +goose Up
-- 0001 Telegram 风险验证基座（P0 Plan §4）：gotd 持久状态 + canonical 消息。
-- 依据冻结设计 02-storage §2.1（R3.1.1）与 §2.3。

CREATE TABLE gotd_sessions (
    account    VARCHAR(8)  NOT NULL COMMENT 'user / bot 逻辑槽',
    data       MEDIUMBLOB  NOT NULL COMMENT 'gotd session opaque blob（不解析、不版本化）',
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (account)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE telegram_update_states (
    account    VARCHAR(8)        NOT NULL,
    user_id    BIGINT UNSIGNED   NOT NULL COMMENT '状态身份：该 auth session 认证的 TG user ID（换号清旧）',
    pts        BIGINT UNSIGNED   NOT NULL DEFAULT 0,
    qts        BIGINT UNSIGNED   NOT NULL DEFAULT 0,
    seq        BIGINT UNSIGNED   NOT NULL DEFAULT 0,
    date       BIGINT            NOT NULL DEFAULT 0,
    updated_at DATETIME(6)       NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (account, user_id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE telegram_channel_states (
    account    VARCHAR(8)      NOT NULL,
    user_id    BIGINT UNSIGNED NOT NULL,
    channel_id BIGINT UNSIGNED NOT NULL,
    pts        BIGINT UNSIGNED NOT NULL DEFAULT 0,
    updated_at DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (account, user_id, channel_id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE telegram_peers (
    account    VARCHAR(8)      NOT NULL,
    peer_type  VARCHAR(8)      NOT NULL COMMENT 'user / chat / channel（裸 ID 空间重叠，身份含类型）',
    peer_id    BIGINT UNSIGNED NOT NULL,
    data       MEDIUMBLOB      NOT NULL COMMENT 'gotd/contrib storage.Peer 序列化（access_hash 在内）',
    username   VARCHAR(64)     NULL COMMENT '展示/索引快照（可变）',
    title      VARCHAR(255)    NULL,
    updated_at DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (account, peer_type, peer_id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE telegram_peer_aliases (
    account    VARCHAR(8)      NOT NULL,
    alias_type VARCHAR(16)     NOT NULL COMMENT 'username / phone',
    alias_value VARCHAR(255)   NOT NULL,
    peer_type  VARCHAR(8)      NOT NULL,
    peer_id    BIGINT UNSIGNED NOT NULL,
    updated_at DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (account, alias_type, alias_value)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE messages (
    id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    chat_type          VARCHAR(8)      NOT NULL COMMENT 'user / chat / channel（ChatRef 原则，02 §1.1）',
    chat_id            BIGINT UNSIGNED NOT NULL,
    message_id         BIGINT UNSIGNED NOT NULL,
    source_type        VARCHAR(24)     NOT NULL COMMENT 'channel_message / discussion_message / bot_reply',
    conversation_id    BIGINT UNSIGNED NULL,
    thread_top_id      BIGINT UNSIGNED NULL COMMENT '讨论线程顶层消息（非线程=自身；NULL=未知）',
    sender_user_id     BIGINT UNSIGNED NULL,
    sender_username    VARCHAR(64)     NULL,
    sender_display_name VARCHAR(255)   NULL,
    text               MEDIUMTEXT      NULL,
    media              JSON            NULL COMMENT '媒体元数据（file_reference 为可刷新缓存引用）',
    ai_meta            JSON            NULL,
    published_at       DATETIME(6)     NOT NULL,
    edited_at          DATETIME(6)     NULL,
    deleted_at         DATETIME(6)     NULL,
    current_revision   INT UNSIGNED    NOT NULL DEFAULT 0,
    embedding_state    VARCHAR(16)     NOT NULL DEFAULT 'pending' COMMENT 'pending/indexed/delete_pending/excluded/error',
    created_at         DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at         DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_chat_message (chat_type, chat_id, message_id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE message_revisions (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    message_id BIGINT UNSIGNED NOT NULL COMMENT '→ messages.id',
    revision   INT UNSIGNED    NOT NULL COMMENT '0=原始版本',
    event_type VARCHAR(8)      NOT NULL COMMENT 'create / edit / delete（immutable，只 INSERT）',
    text       MEDIUMTEXT      NULL COMMENT 'delete 事件为 NULL（事件本身即内容）',
    media      JSON            NULL,
    ai_meta    JSON            NULL,
    edited_at  DATETIME(6)     NULL,
    created_at DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_message_revision (message_id, revision)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- +goose Down
DROP TABLE IF EXISTS message_revisions;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS telegram_peer_aliases;
DROP TABLE IF EXISTS telegram_peers;
DROP TABLE IF EXISTS telegram_channel_states;
DROP TABLE IF EXISTS telegram_update_states;
DROP TABLE IF EXISTS gotd_sessions;
