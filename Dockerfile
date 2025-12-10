# СТАДИЯ СБОРКИ (Build Stage)
FROM golang:1.25.3-alpine AS builder

# Устанавливаем рабочую директорию внутри контейнера
WORKDIR /app

# Копируем go.mod и go.sum и загружаем зависимости
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходный код
COPY . .

# Компилируем Go-приложение в статический бинарник
RUN CGO_ENABLED=0 go build -o /bot main.go
# ------------------ СТАДИЯ СБОРКИ ЗАВЕРШЕНА ----------------------


# ------------------ СТАДИЯ ЗАПУСКА ------------------------------
# СТАДИЯ ЗАПУСКА (Run Stage)
FROM scratch 

# 1. Копируем корневые сертификаты (исправление ошибки TLS)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# 2. Копируем JSON-ключ сервисного аккаунта! <--- НОВАЯ СТРОКА
COPY credentials.json /credentials.json 

# 3. Копируем скомпилированный бинарник
COPY --from=builder /bot /bot

# Задаем команду для запуска
ENTRYPOINT ["/bot"]