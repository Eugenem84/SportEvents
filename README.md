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

TODO Phase 4 (VK Adapter) — код готов и покрыт тестами: POST /vk/callback (confirmation и secret), message_new и callback-кнопки (message_event), from_id → identity, peer_id → ChatChannel, ответ в беседу с keyboard, идемпотентность повторного callback. Осталась только разовая ручная настройка реального сообщества VK (шаги ниже).

TODO Phase 5 (Подключение беседы) — код готов и покрыт тестами: в беседе с ботом администратор пишет «подключить» → проверка прав через VK API (`messages.getConversationMembers`) → Chat + ChatChannel + первый ChatAdmin одной транзакцией (`chat.Connect`, идемпотентно, гонка повторного callback) → сообщение об успехе. Следующий этап — TODO Phase 6, пользовательский сценарий (список игр, запись, очередь, отмена).

Запуск:

Образ не компилирует Go: бинарь `bin/server` собирается на машине разработчика, а Docker только упаковывает его в Alpine. Так сборка на слабых VPS (1 vCPU / 1 ГБ RAM) вместо часов занимает секунды.

make build
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

make build && APP_PORT=8080 POSTGRES_PORT=5432 docker compose up --build

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
3. «Управление → Работа с API → Callback API»: указать URL `https://sportevent.dev.medovf2h.beget.tech/vk/callback` и версию API `5.199` — совпадает с `vkAPIVersion` в `internal/vk/client.go`.
4. Включить типы событий сообщества: `message_new` и `message_event`.
5. Confirmation-строку, которую VK показывает в этом же разделе, занести в `VK_CONFIRMATION_TOKEN`. Если включён секретный ключ — его значение в `VK_SECRET`.
6. `VK_GROUP_ID` — id сообщества (положительное число из адреса `https://vk.com/club<id>`).
7. Добавить бота в беседу и выдать ему права администратора беседы — без них VK не отдаёт список участников, по которому проверяются права инициатора. После этого администратор беседы пишет боту «подключить»: беседа регистрируется, а он становится администратором Chat. Подключить может только администратор этой беседы.

Локально VK не ходит на `localhost`: нужен публичный HTTPS-туннель на порт приложения или деплой на домашний сервер. В docker-compose VK-переменные идут из `.env` через `VK_*` (см. `.env.example`); без них сервис поднимается, но `/vk/callback` отключён. На домашнем контуре нужны `VK_TOKEN` **и** `VK_CONFIRMATION_TOKEN` (см. DEPLOY.md).

Деплой (домашний сервер)

Канонический контур — домашний сервер за VPS-шлюзом: https://sportevent.dev.medovf2h.beget.tech
(Go-процесс + PostgreSQL 16, TLS терминирует общий Caddy дома, порты проекта не публикуются).
Полный регламент, бэкап и правила «нельзя» — DEPLOY.md.

    # на домашнем сервере (с Mac из домашней сети: ssh home-server)
    bash /opt/projects/sportevents/deploy.sh   # git pull + go build в контейнере golang + docker compose up -d --build
    bash /opt/projects/sportevents/smoke.sh    # /health, HTTP->HTTPS, БД, VK, изоляция портов

    # эквивалент с Mac: make deploy-home

Прежний контур на Beget-VPS (sport-events.dev.medovf2h.beget.tech, 159.194.252.9) признан
недоверенным (подозрение на взлом) и удалён 17.09.2026 — ничего оттуда не переносилось.
Разделы ниже (`make deploy`, `docker-compose.prod.yml`, `Caddyfile`) оставлены как legacy.

Legacy: деплой на Beget-VPS

На слабом VPS (1 vCPU / 1 ГБ RAM) Go-компиляция в контейнере может идти часами или падать по OOM, поэтому код компилируется на машине разработчика, а на сервер кладётся готовый бинарь. Образ собирается без компиляции Go.

make deploy

Что делает `make deploy`:

1. `vps-build` — кросс-компиляция под linux/amd64 в `bin/server`;
2. `scp bin/server root@<хост>:~/SportEvents/bin/server`;
3. на VPS: `git pull --ff-only` и
   `docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build`.

Хост/пользователь/каталог переопределяются аргументами (по умолчанию root@ebszsvlknf / ~/SportEvents):

make deploy VPS_HOST=159.194.252.9 VPS_USER=root VPS_DIR=~/SportEvents

Если есть SSH-алиас из `~/.ssh/config`, задайте `VPS_HOST=<алиас>`.

Вручную (без make):

    # с машины разработчика
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/server ./cmd/server
    scp bin/server root@<хост>:~/SportEvents/bin/server

    # на VPS
    cd ~/SportEvents && git pull --ff-only
    docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build

Если предыдущая сборка зависла (go build на 1 ГБ RAM), сначала на VPS остановите её и уберите полу-собранные образы:

    docker compose -f docker-compose.yml -f docker-compose.prod.yml down
    docker image prune -f

Проверка после деплоя (публичный домен):

    curl -i https://sportevent.dev.medovf2h.beget.tech/health
    curl -i -X POST https://sportevent.dev.medovf2h.beget.tech/vk/callback \
      -H 'Content-Type: application/json' \
      -d '{"type":"confirmation"}'

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
