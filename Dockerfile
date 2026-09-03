FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /server ./cmd/server

FROM alpine:3.21

WORKDIR /app
COPY --from=build /server /app/server
EXPOSE 8080
USER nobody
CMD ["/app/server"]
