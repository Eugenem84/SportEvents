-- Расписание игр чата: по каким дням недели и в какое время игры идут
-- регулярно. С расписанием «создать игру» не требует ввода даты.
--
-- weekday: 0 — воскресенье … 6 — суббота (как time.Weekday в Go и DOW в
-- PostgreSQL, чтобы не конвертировать туда-сюда).
-- minutes: время суток в минутах от полуночи (0..1439) — в поясе чата;
-- сами игры по-прежнему хранятся в UTC.
CREATE TABLE chat_game_slots (
    chat_id    BIGINT NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    weekday    SMALLINT NOT NULL CHECK (weekday BETWEEN 0 AND 6),
    minutes    SMALLINT NOT NULL CHECK (minutes BETWEEN 0 AND 1439),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, weekday)
);
