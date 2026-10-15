# --- build stage ---
FROM golang:1.21-alpine AS build
WORKDIR /src
# Module deps first (better layer caching), then source.
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/staging-platform .

# --- runtime stage ---
# docker CLI + git run discovery; docker-cli-compose provides `docker compose`,
# used only to stop/start a whole env (env power page).
FROM alpine:3.20
RUN apk add --no-cache docker-cli docker-cli-compose git ca-certificates tzdata openssh-client
WORKDIR /app
COPY --from=build /out/staging-platform /app/staging-platform
COPY config.example.json /app/config.example.json
# App data (SQLite DB) lives here; mount a volume over it for persistence.
RUN mkdir -p /app/data
EXPOSE 8088
ENTRYPOINT ["/app/staging-platform", "-config", "/app/config.json"]
