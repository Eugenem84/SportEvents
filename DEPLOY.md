# DEPLOY — SportEvents (sportevent.dev.medovf2h.beget.tech)

Регламент проекта в инфраструктуре домашнего сервера. Вся инфраструктура стенда —
репозиторий `home-server-vps` ([`docs/10-sportevents.md`](../../docs/10-sportevents.md)).
Копия этого файла лежит в репозитории проекта: `SportEvents/DEPLOY.md`.

## Где живёт

- Домен: `sportevent.dev.medovf2h.beget.tech` (A → `90.156.169.123`).
- Сервер: домашний Ubuntu `192.168.2.207`; каталог `/opt/projects/sportevents`,
  код — `/opt/projects/sportevents/src` (git-клон `SportEvents`, ветка `main`).
- Путь запроса: интернет → VPS `90.156.169.123` (DNAT 80/443, SNAT, MSS-clamp) →
  WireGuard-туннель (`10.10.0.1` ↔ `10.10.0.2`) → Caddy дома (`10.10.0.2:80` / `:8443`,
  TLS Let's Encrypt) → `infra_net` → `sportevents-app:8080`.
- TLS терминирует Caddy; приложение работает по HTTP внутри docker-сети.
- PostgreSQL: контейнер `sportevents-db` (`postgres:16-alpine`) в сети `sportevents_net`,
  наружу **не публикуется**.
- Эндпоинты: `GET /health` (пинг БД, `ok`/503), `POST /vk/callback` (включён, только если
  заданы `VK_TOKEN` **и** `VK_CONFIRMATION_TOKEN`).
- Миграции вшиты в бинарь (`internal/postgres/migrations`, `embed`) и применяются при старте.

## Секреты

- `/opt/projects/sportevents/.env` — `DB_NAME`, `DB_USER`, `DB_PASSWORD`,
  `VK_GROUP_ID`, `VK_TOKEN`, `VK_CONFIRMATION_TOKEN`, `VK_SECRET`: права `600`, в git не попадает.
- В git — только `projects/sportevents/.env.example` (без значений).
- Никогда не печатать содержимое `.env`, токены и пароли в логах и отчётах.

## Сборка и деплой

```bash
bash /opt/projects/sportevents/deploy.sh
```

Что делает: `git -C src pull --ff-only` → `go build` в одноразовом контейнере
`golang:1.24-alpine` (linux/amd64, кеш модулей в томе `sportevents_go_mod`) →
`docker compose up -d --build` → `docker compose ps`.

Вручную, если нужно:

```bash
cd /opt/projects/sportevents
git -C src pull --ff-only
docker run --rm -v "$PWD/src":/src -w /src golang:1.24-alpine \
  sh -c 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/server ./cmd/server'
docker compose up -d --build
```

Откат: `git -C src checkout <предыдущий-коммит>` → тот же `go build` → `docker compose up -d --build`.
Схема БД совместима назад только если миграции не менялись — перед обновлением снять дамп.

## Проверка

```bash
bash /opt/projects/sportevents/smoke.sh                # на домашнем сервере (публичный домен)
bash /opt/projects/sportevents/smoke.sh --local        # через Caddy дома, без DNS
ssh dev-vps 'curl -sSI https://sportevent.dev.medovf2h.beget.tech/health'   # снаружи
```

После смены `VK_*` в `.env` — перезапуск приложения: `docker compose up -d --force-recreate sportevents-app`
(значения читаются из окружения контейнера при старте).

## VK

1. Сообщество: «Управление → Работа с API → Callback API», URL
   `https://sportevent.dev.medovf2h.beget.tech/vk/callback`, версия API `5.199`
   (совпадает с `vkAPIVersion` в `internal/vk/client.go`).
2. События: `message_new` и `message_event`.
3. Строку подтверждения из этого же раздела — в `VK_CONFIRMATION_TOKEN`; если включён
   секретный ключ, его значение — в `VK_SECRET`.
4. Проверка: `bash smoke.sh` (раздел 2) — ответ на `{"type":"confirmation"}` должен совпасть
   со строкой из `.env`, событие с чужим `group_id` — получить `403`, а своё событие
   (`message_new`, `out=1`) — `200 ok`. Если в сообществе включён секретный ключ, дополнительно
   проверяется отказ по чужому `secret` (`403`); при пустом `VK_SECRET` эта проверка не делается —
   приложение обязано принимать события без ключа.
5. Подключение беседы: бот должен состоять в беседе **администратором** — иначе
   `messages.getConversationMembers` (проверка прав инициатора) и
   `messages.getConversationsById` (название беседы) вернут ошибку. Администратор беседы
   пишет боту «подключить»; в ответ приходит «Беседа «…» подключена». Токену нужен
   доступ `messages` (тот же, что для отправки ответов).

## Бэкап и восстановление

```bash
cd /opt/projects/sportevents
docker compose exec -T sportevents-db pg_dump -U sportevents -d booking -Fc \
  > /opt/backups/pg/sportevents-$(date +%F).dump
# проверка дампа на временной базе:
docker compose exec -T sportevents-db psql -U sportevents -d postgres -c 'CREATE DATABASE se_restore_check'
docker compose exec -T sportevents-db pg_restore -U sportevents -d se_restore_check --clean --if-exists < <dump>
docker compose exec -T sportevents-db psql -U sportevents -d postgres -c 'DROP DATABASE se_restore_check'
```

Автоматических бэкапов на контуре пока нет (этап 9 инфра-репозитория): дампы снимаются
вручную в `/opt/backups/pg` (диск ОС `/dev/sda5`).

## Нельзя

- публиковать порты на хост (включая PostgreSQL), использовать `privileged` или `network_mode: host`;
- трогать чужие проекты (старый Ledger-Craft с Traefik на `0.0.0.0:443`/`:8080` и Postgres на `0.0.0.0:5433`)
  и любые диски, кроме диска ОС `/dev/sda5`;
- коммитить `.env` и значения `VK_*` (репозиторий проекта публичный);
- менять `Caddyfile`, не записав проект в `projects/REGISTRY.md` инфра-репозитория.
