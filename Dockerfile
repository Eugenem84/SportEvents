# Бинарь собирается на машине разработчика (make build / make vps-build),
# а этот образ только упаковывает готовый bin/server в Alpine.
#
# Это убирает компиляцию Go на слабых VPS (1 vCPU / 1 ГБ RAM), где
# `go build` может идти часами или падать по OOM. Теперь `up --build`
# просто копирует бинарь в образ и занимает секунды.
#
#   make build
#   docker compose up --build

FROM alpine:3.21

WORKDIR /app
COPY bin/server /app/server
EXPOSE 8080
USER nobody
CMD ["/app/server"]
