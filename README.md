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

Этапы 1–5 (TODO Phase 1–3): Event Service поверх PostgreSQL и Booking Service(запись, очередь, отмена, FIFO promotion, уведомление)— готово и покрыто интеграционными тестами. Следующий этап — VK Adapter(TODO Phase 4): /vk/callback, кнопки, подключение беседы.

Запуск:

docker compose up --build

Проверка: GET http://localhost:8082/health → ok (пинг БД)

Тесты Event и Booking Service (нужен запущенный Postgres):

go test ./internal/event/ ./internal/booking/ -count=1

Без локального Go — из каталога проекта, с сетью Compose:

docker run --rm --network sportevents_default \
  -e DATABASE_URL=postgres://postgres:postgres@postgres:5432/booking?sslmode=disable \
  -v "$PWD":/src -w /src golang:1.24-alpine \
  go test ./internal/event/ ./internal/booking/ -count=1

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
