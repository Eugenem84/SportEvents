-- Номер места в составе игры.
--
-- Место закрепляется за человеком, а при отписке просто освобождается: следующий
-- записавшийся занимает именно этот номер, поэтому нумерация в анонсе не
-- сдвигается. Номер выдаёт приложение (наименьший свободный), а уникальность
-- гарантирует индекс: одно место — один человек.
ALTER TABLE bookings ADD COLUMN seat_no INTEGER;

-- Существующие подтверждённые записи нумеруем по порядку записи, чтобы данные
-- до этой миграции тоже попали в нумерацию.
UPDATE bookings b
SET seat_no = numbered.seat
FROM (
    SELECT id,
           row_number() OVER (PARTITION BY event_id ORDER BY created_at, id) AS seat
    FROM bookings
    WHERE status = 'confirmed'
) AS numbered
WHERE b.id = numbered.id;

CREATE UNIQUE INDEX bookings_event_id_seat_no_confirmed_uidx
    ON bookings (event_id, seat_no)
    WHERE status = 'confirmed' AND seat_no IS NOT NULL;

ALTER TABLE bookings
    ADD CONSTRAINT bookings_seat_no_positive CHECK (seat_no IS NULL OR seat_no >= 1);
