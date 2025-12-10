# СТАДИЯ СБОРКИ (Build Stage)
# Используем образ Go 1.25.3 для компиляции
FROM golang:1.25.3-alpine AS builder

# Устанавливаем рабочую директорию
WORKDIR /app

# Копируем go.mod и go.sum, загружаем зависимости
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходный код
COPY . .

# Компилируем Go-приложение в статический бинарник
# CGO_ENABLED=0 критически важен, он делает бинарник независимым от системных библиотек
RUN CGO_ENABLED=0 go build -o /bot main.go


# СТАДИЯ ЗАПУСКА (Run Stage)
# Используем абсолютно пустой образ (scratch)
FROM scratch 

# Копируем скомпилированный бинарник
COPY --from=builder /bot /bot

# Задаем команду для запуска
# Путь короче, потому что в scratch нет стандартных папок типа /usr/local/bin
ENTRYPOINT ["/bot"]