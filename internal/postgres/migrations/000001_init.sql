CREATE TABLE chats (
    id         BIGSERIAL PRIMARY KEY,
    title      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE chat_channels (
    id               BIGSERIAL PRIMARY KEY,
    chat_id          BIGINT NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    platform         TEXT NOT NULL,
    external_chat_id TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (platform, external_chat_id)
);

CREATE TABLE users (
    id           BIGSERIAL PRIMARY KEY,
    display_name TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_identities (
    id               BIGSERIAL PRIMARY KEY,
    user_id          BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    platform         TEXT NOT NULL,
    external_user_id TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (platform, external_user_id)
);

CREATE TABLE chat_admins (
    chat_id BIGINT NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (chat_id, user_id)
);

CREATE TABLE events (
    id         BIGSERIAL PRIMARY KEY,
    chat_id    BIGINT NOT NULL REFERENCES chats (id) ON DELETE RESTRICT,
    starts_at  TIMESTAMPTZ NOT NULL,
    title      TEXT NOT NULL,
    location   TEXT NOT NULL DEFAULT '',
    capacity   INTEGER NOT NULL CHECK (capacity >= 1),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX events_chat_id_starts_at_idx ON events (chat_id, starts_at);

CREATE TABLE bookings (
    id                BIGSERIAL PRIMARY KEY,
    event_id          BIGINT NOT NULL REFERENCES events (id) ON DELETE RESTRICT,
    player_name       TEXT NOT NULL,
    phone             TEXT,
    user_id           BIGINT REFERENCES users (id) ON DELETE RESTRICT,
    booked_by_user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    status            TEXT NOT NULL CHECK (status IN ('confirmed', 'waitlist', 'cancelled')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX bookings_event_id_status_created_at_idx
    ON bookings (event_id, status, created_at);

CREATE UNIQUE INDEX bookings_event_id_user_id_active_uidx
    ON bookings (event_id, user_id)
    WHERE user_id IS NOT NULL AND status IN ('confirmed', 'waitlist');
