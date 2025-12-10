# СТАДИЯ СБОРКИ (Build Stage) - Без изменений
FROM golang:1.25.3-alpine AS builder
# ... (шаги сборки, компиляция)
RUN CGO_ENABLED=0 go build -o /bot main.go


# СТАДИЯ ЗАПУСКА (Run Stage) - Вносим изменение!
FROM scratch 

# ОЧЕНЬ ВАЖНО: Копируем корневые сертификаты из образа сборки
# Это решает проблему "certificate signed by unknown authority"
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Копируем скомпилированный бинарник
COPY --from=builder /bot /bot

# Задаем команду для запуска
ENTRYPOINT ["/bot"]