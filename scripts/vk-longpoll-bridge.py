#!/usr/bin/env python3
"""Мостик: Bots Long Poll → POST /vk/callback (локальная разработка VK).

Зачем: VK доставляет события либо в Callback API (нужен публичный HTTPS:
домен, туннель или деплой), либо через Bots Long Poll, который можно опрашивать
откуда угодно — в том числе с ноутбука. Мостик читает Long Poll и пересылает
обновления локальному обработчику ровно в том формате, в котором VK шлёт
Callback API (group_id, type, event_id, object), поэтому приложение не знает,
откуда пришли события.

Что должно быть включено в сообществе: «Управление → Работа с API → Long Poll API»
и события message_new и message_event (без второго не работают callback-кнопки —
например, кнопки настроек расписания). Чтобы увидеть то, что мини-приложение
отправило через VKWebAppSendPayload, нужен ещё тип события app_payload и
разрешение «Запуск приложения из сообщества» в настройках приложения.

Запуск:

    set -a; . ./.env; set +a
    BRIDGE_PEER_ID=2000000002 python3 scripts/vk-longpoll-bridge.py

Переменные окружения: VK_TOKEN и VK_GROUP_ID (берутся из .env), BRIDGE_TARGET
(по умолчанию http://localhost:8082/vk/callback), BRIDGE_PEER_ID — беседа, чьи
события уходят в приложение (пусто — пересылать все обновления сообщества).
"""
import json
import os
import time
import urllib.parse
import urllib.request

TOKEN = os.environ.get("VK_TOKEN", "")
GROUP = os.environ.get("VK_GROUP_ID", "")
TARGET = os.environ.get("BRIDGE_TARGET", "http://localhost:8082/vk/callback")
ONLY_PEER = os.environ.get("BRIDGE_PEER_ID", "")

# Обновления, которые умеет обрабатывать приложение.
FORWARD = {"message_new", "message_event", "app_payload"}


def api(method, params):
    data = urllib.parse.urlencode({"access_token": TOKEN, "v": "5.199", **params}).encode()
    with urllib.request.urlopen("https://api.vk.com/method/" + method, data, timeout=30) as r:
        return json.load(r)


def long_poll_server():
    """Адрес Long Poll, ключ и текущий ts сообщества."""
    resp = api("groups.getLongPollServer", {"group_id": GROUP})["response"]
    return resp["server"], resp["key"], resp["ts"]


def peer_of(update):
    """peer_id обновления: у message_new он внутри message, у message_event —
    прямо в объекте."""
    obj = update.get("object") or {}
    if update.get("type") == "message_new":
        return (obj.get("message") or {}).get("peer_id")
    return obj.get("peer_id")


def describe(update):
    """Строка для лога: видно, что пришло и из какой беседы."""
    obj = update.get("object") or {}
    msg = obj.get("message") or {}
    if update.get("type") == "message_new":
        return f"peer={msg.get('peer_id')} from={msg.get('from_id')} text={msg.get('text')!r}"
    if update.get("type") == "app_payload":
        # То, что мини-приложение отправило в сообщество: печатаем как есть,
        # состав объекта у VK описан скупо, а угадывать поля не хочется.
        return (
            f"user={obj.get('user_id')} app={obj.get('app_id')} "
            f"payload={obj.get('payload')!r}"
        )
    return (
        f"peer={obj.get('peer_id')} user={obj.get('user_id')} "
        f"cmid={obj.get('conversation_message_id')} payload={obj.get('payload')!r}"
    )


def main():
    if not TOKEN or not GROUP:
        raise SystemExit("VK_TOKEN и VK_GROUP_ID обязательны (источник: .env)")

    server, key, ts = long_poll_server()
    print(f"long poll {server} ts={ts} peer={ONLY_PEER or 'все'} -> {TARGET}", flush=True)

    heartbeat = time.time()
    failures = 0
    while True:
        url = f"{server}?act=a_check&key={key}&ts={ts}&wait=25"
        try:
            with urllib.request.urlopen(url, timeout=60) as r:
                update = json.load(r)
            failures = 0
        except Exception as e:  # noqa: BLE001 — мостик не должен падать
            failures += 1
            print(f"[{time.strftime('%H:%M:%S')}] long poll error ({failures}): {e}", flush=True)
            time.sleep(3)
            if failures >= 3:
                # Сеть отвалилась надолго: берём новый сервер и ключ.
                server, key, ts = long_poll_server()
                print(f"[{time.strftime('%H:%M:%S')}] long poll заново {server} ts={ts}", flush=True)
            continue

        # VK отвечает {"failed": N}, когда ts устарел или ключ недействителен.
        # Без этого обработчика мостик молча крутился бы на мёртвом ts.
        if update.get("failed"):
            why = update["failed"]
            ts = update.get("ts", ts)
            if why != 1:
                server, key, ts = long_poll_server()
            print(f"[{time.strftime('%H:%M:%S')}] long poll failed={why}, продолжаю с ts={ts}", flush=True)
            continue

        ts = update.get("ts", ts)

        if not update.get("updates") and time.time() - heartbeat > 300:
            # Раз в пять минут видно, что мостик жив и опрашивает VK.
            heartbeat = time.time()
            print(f"[{time.strftime('%H:%M:%S')}] жив, ts={ts}", flush=True)

        for u in update.get("updates", []):
            heartbeat = time.time()
            if u.get("type") not in FORWARD:
                continue
            peer = peer_of(u)
            print(f"[{time.strftime('%H:%M:%S')}] {u.get('type')} {describe(u)}", flush=True)
            # app_payload приходит сообществу, а не беседе: peer_id у него нет,
            # и фильтр по выбранной беседе не должен его съедать.
            if ONLY_PEER and peer is not None and str(peer) != str(ONLY_PEER):
                continue

            req = urllib.request.Request(
                TARGET, data=json.dumps(u).encode(), headers={"Content-Type": "application/json"}
            )
            try:
                with urllib.request.urlopen(req, timeout=30) as rr:
                    print(f"  -> local {rr.status} {rr.read()[:60]!r}", flush=True)
            except Exception as e:  # noqa: BLE001 — мостик не должен падать
                print(f"  -> local error: {e}", flush=True)


if __name__ == "__main__":
    main()
