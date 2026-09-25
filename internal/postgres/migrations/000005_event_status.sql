-- Отмена игры администратором: событие остаётся в базе (на него ссылаются
-- записи и анонс в беседе), но перестаёт быть доступным для записи и пропадает
-- из списков предстоящих игр.
ALTER TABLE events
    ADD COLUMN status TEXT NOT NULL DEFAULT 'scheduled'
        CHECK (status IN ('scheduled', 'cancelled'));

-- Предстоящие игры выбираются по (chat_id, starts_at) среди запланированных,
-- поэтому статус входит в индекс.
DROP INDEX events_chat_id_starts_at_idx;
CREATE INDEX events_chat_id_status_starts_at_idx ON events (chat_id, status, starts_at);
