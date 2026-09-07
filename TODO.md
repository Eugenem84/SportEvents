TODO

Phase 1 — Foundation

* Создать Go module — готово
* Создать Dockerfile — готово
* Создать docker-compose.yml — готово
* Запустить PostgreSQL — готово
* Подключить pgx — готово
* Настроить .env (не коммитить секреты) — готово (.env.example)
* Миграции: chats, chat_channels, users, user_identities, chat_admins, events, bookings — готово
* Индексы и unique из ARCHITECTURE.md — готово

Phase 2 — Events

* Создание event (starts_at timestamptz) — готово
* Получение ближайших events — готово
* Получение event по ID — готово
* Принадлежность event к Chat — готово
* Запрет уменьшить capacity ниже confirmed_count — готово
* Тесты Event Service — готово

Phase 3 — Bookings

* Создание booking — готово
* Повторная активная запись по user_id — готово (проверка в Create + частичный unique индекс)
* Гость без user_id — готово (тест: два гостя с одним именем допустимы)
* capacity, SELECT FOR UPDATE, confirmed, waitlist — готово
* Отмена и FIFO promotion — готово（код Cancel + CancelResult.Promoted + тест TestCancelPromotesFIFO)
* Уведомление после promotion（identity / booked_by / беседа）— готово（интерфейс Notifier, Cancel вызывает NotifyPromotion после commit; тест TestNotifyPromotionNotSilentForGuest. Реализация отправки для VK — адаптер / Phase 4）
* Добавление игрока другим пользователем — готово (Create с booked_by_user_id; проверка прав — адаптер / Phase 7)
* Удаление игрока администратором — частично: операция Cancel есть, проверка chat_admins — Phase 7
* Тесты, в том числе конкурентная запись— готово（create/дубль/гость/отмена/FIFO promotion/уведомление/конкурентная запись; нужен запущенный Postgres）

Phase 4 — VK

* VK community, token, Events API
* /vk/callback: confirmation + secret
* message и callback button
* from_id, peer_id
* User identity и ChatChannel
* Ответ в беседу, keyboard
* Идемпотентность повторного callback

Phase 5 — Подключение беседы

* Бот в существующей беседе
* Команда подключения
* Права инициатора (VK API только в адаптере)
* Chat + ChatChannel + первый ChatAdmin
* Сообщение об успехе

Phase 6 — Пользовательский сценарий

* «Игры», свободные места, очередь
* Записаться / в очередь / мои записи / отмена

Phase 7 — Администратор

* Команды админа, проверка chat_admins
* Создание event, списки, ручное добавление и удаление

Phase 8 — Testing

* Unit и integration с PostgreSQL
* Concurrent booking
* VK adapter tests
* Полный сценарий в реальном VK

Phase 9 — Future (не до завершения V1)

* Web, MAX, Telegram, mobile
* Карта площадок, статистика, оплата
* Аккаунты сайта, сложные роли
* Интерфейс Messenger
* Redis

Отложено до реальной боли

* Отдельная сущность players
* Отдельная таблица venues
* Несколько каналов на один Chat (таблица chat_channels уже заложена)
* Отдельное поле timezone у Chat
