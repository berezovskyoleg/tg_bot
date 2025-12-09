# СТАДИЯ СБОРКИ (Build Stage): Используем Go для компиляции
FROM golang:1.25.3-alpine AS builder

# Устанавливаем рабочую директорию внутри контейнера
WORKDIR /app

# Копируем go.mod и go.sum и загружаем зависимости
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходный код
COPY . .

# Компилируем Go-приложение в статический бинарник
# -o /bot: имя бинарника будет 'bot'
RUN go build -o /bot main.go


# СТАДИЯ ЗАПУСКА (Run Stage): Используем минимальный образ Alpine для легкости
FROM alpine:latest

# Копируем скомпилированный бинарник из стадии сборки
COPY --from=builder /bot /usr/local/bin/bot

# Задаем команду для запуска
ENTRYPOINT ["/usr/local/bin/bot"]