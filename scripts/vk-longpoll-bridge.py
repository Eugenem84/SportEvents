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
например, кнопки настроек расписания).

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
FORWARD = {"message_new", "message_event"}


def api(method, params):
    data = urllib.parse.urlencode({"access_token": TOKEN, "v": "5.199", **params}).encode()
    with urllib.request.urlopen("https://api.vk.com/method/" + method, data, timeout=30) as r:
        return json.load(r)


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
    return (
        f"peer={obj.get('peer_id')} user={obj.get('user_id')} "
        f"cmid={obj.get('conversation_message_id')} payload={obj.get('payload')!r}"
    )


def main():
    if not TOKEN or not GROUP:
        raise SystemExit("VK_TOKEN и VK_GROUP_ID обязательны (источник: .env)")

    resp = api("groups.getLongPollServer", {"group_id": GROUP})["response"]
    server, key, ts = resp["server"], resp["key"], resp["ts"]
    print(f"long poll {server} ts={ts} peer={ONLY_PEER or 'все'} -> {TARGET}", flush=True)

    while True:
        url = f"{server}?act=a_check&key={key}&ts={ts}&wait=25"
        with urllib.request.urlopen(url, timeout=60) as r:
            update = json.load(r)
        ts = update.get("ts", ts)

        for u in update.get("updates", []):
            if u.get("type") not in FORWARD:
                continue
            peer = peer_of(u)
            print(f"[{time.strftime('%H:%M:%S')}] {u.get('type')} {describe(u)}", flush=True)
            if ONLY_PEER and str(peer) != str(ONLY_PEER):
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
