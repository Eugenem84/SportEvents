-- Ссылка на сообщение-анонс игры во внешнем чате.
--
-- Список записавшихся показывается в одном и том же сообщении и обновляется
-- через messages.edit, поэтому приложению нужно помнить id этого сообщения.
-- Таблица платформо-независима: (event_id, platform) — ключ, внешний чат и id
-- сообщения — платформенные детали.
CREATE TABLE event_announcements (
    event_id         BIGINT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    platform         TEXT NOT NULL,
    external_chat_id TEXT NOT NULL,
    message_id       BIGINT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, platform)
);
