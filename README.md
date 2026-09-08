Event Booking

Сервис записи людей на мероприятия через мессенджеры.

Первый сценарий — запись на волейбольные игры через VK.

Позже тот же backend может обслуживать другие виды мероприятий (футбол, самбо, тренировки, турниры) и другие каналы. Они не входят в первую версию.

Документы

* README.md — продукт и статус
* ARCHITECTURE.md — домен, схема, инварианты
* DEVELOPMENT.md — как наращивать код
* TODO.md — чеклист работ

Основная идея

Организатор и группа людей общаются в мессенджере.

Администратор подключает бота к существующей беседе. После этого участники могут:

* смотреть ближайшие мероприятия;
* записываться;
* отменять запись;
* добавлять других участников;
* видеть очередь;
* получать место после освобождения места.

Администратор может:

* создавать мероприятия;
* смотреть список участников;
* вручную добавлять участников;
* удалять участников;
* управлять очередью.

Архитектура (кратко)

VK → VK Adapter → Go backend (Event / Booking) → PostgreSQL

Бизнес-логика записи не зависит от VK. Другие клиенты (MAX, Telegram, web) в V1 не делаются; когда понадобятся, они вызывают те же сервисы.

Подробности — в ARCHITECTURE.md.

Технологии

* Go
* PostgreSQL
* pgx, SQL вручную (без ORM)
* Docker, Docker Compose
* VK Bot API
* HTTP/JSON

Статус

TODO Phase 1–3 (Event Service поверх PostgreSQL, Booking Service: запись, очередь, отмена, FIFO promotion, уведомление) — готово и покрыто интеграционными тестами.

TODO Phase 4 (VK Adapter) — код готов и покрыт тестами: POST /vk/callback (confirmation и secret), message_new и callback-кнопки (message_event), from_id → identity, peer_id → ChatChannel, ответ в беседу с keyboard, идемпотентность повторного callback. Осталась только разовая ручная настройка реального сообщества VK (шаги ниже). Следующий этап — TODO Phase 5, подключение беседы.

Запуск:

docker compose up --build

Проверка: GET http://localhost:8082/health → ok (пинг БД)

Тесты (нужен запущенный Postgres для event/booking/chat, vk — без него):

go test ./internal/event/ ./internal/booking/ ./internal/chat/ ./internal/vk/ -count=1

Без локального Go — из каталога проекта, с сетью Compose:

docker run --rm --network sportevents_default \
  -e DATABASE_URL=postgres://postgres:postgres@postgres:5432/booking?sslmode=disable \
  -v "$PWD":/src -w /src golang:1.24-alpine \
  go test ./internal/event/ ./internal/booking/ ./internal/chat/ ./internal/vk/ -count=1

Порты на хосте по умолчанию: приложение 8082, Postgres 5434 (внутри сети Compose Postgres слушает 5432). Так меньше конфликтов с другими локальными контейнерами.

APP_PORT=8080 POSTGRES_PORT=5432 docker compose up --build

Шаблон переменных: `.env.example`. Секреты не коммитятся. Для `go run` с хоста:

DATABASE_URL=postgres://postgres:postgres@localhost:5434/booking?sslmode=disable

DATABASE_URL=postgres://postgres:postgres@postgres:5432/booking?sslmode=disable
VK_GROUP_ID=
VK_TOKEN=
VK_CONFIRMATION_TOKEN=
VK_SECRET=

VK_SECRET — ключ проверки callback, если его требует актуальная документация VK, отдельно от confirmation-строки.

Параметры VK API сверять с официальной документацией перед интеграцией.

Настройка VK (разово, вручную)

1. Создать сообщество VK (или использовать существующее). В «Управление → Сообщения» включить сообщения и бота.
2. Получить ключ доступа сообщества с правом `messages` («Управление → Работа с API → Ключи доступа») — это `VK_TOKEN`.
3. «Управление → Работа с API → Callback API»: указать URL `https://<домен>/vk/callback` (например, через Caddy) и версию API `5.199` — совпадает с `vkAPIVersion` в `internal/vk/client.go`.
4. Включить типы событий сообщества: `message_new` и `message_event`.
5. Confirmation-строку, которую VK показывает в этом же разделе, занести в `VK_CONFIRMATION_TOKEN`. Если включён секретный ключ — его значение в `VK_SECRET`.
6. `VK_GROUP_ID` — id сообщества (положительное число из адреса `https://vk.com/club<id>`).

Локально VK не ходит на `localhost`: нужен публичный HTTPS-туннель на порт приложения или деплой на VPS. В docker-compose VK-переменные идут из `.env` через `VK_*` (см. `.env.example`); без них сервис поднимается, но `/vk/callback` отключён.

Первая версия

Входит:

* PostgreSQL и один Go-процесс;
* внутренние Chat, события, записи, очередь;
* связь VK-беседы с Chat;
* минимальные внутренние пользователи (identity из VK, не аккаунты сайта);
* администраторы чата;
* VK adapter;
* Docker Compose.

Не входит:

* оплата;
* регистрация / логин на сайте;
* сложные роли;
* MAX, Telegram, web, mobile;
* полноценная веб-админка;
* статистика;
* карта площадок.

Критерий готовности V1 — в DEVELOPMENT.md.
