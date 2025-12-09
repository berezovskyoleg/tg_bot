package main

import (
	"fmt"
	"log"
	"os"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func main() {
	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		log.Fatal("Переменная окружения TELEGRAM_BOT_TOKEN не задана")
	}

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Panic(err)
	}

	log.Printf("Авторизация на аккаунте %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	// --- ИНИЦИАЛИЗАЦИЯ КЛАВИАТУРЫ ---
	buttonID := tgbotapi.NewInlineKeyboardButtonData("Показать мой ID", "show_my_id")
	buttonGo := tgbotapi.NewInlineKeyboardButtonURL("Сайт Go", "https://golang.org/")
	keyboardRow := tgbotapi.NewInlineKeyboardRow(buttonID, buttonGo)
	inlineKeyboard := tgbotapi.NewInlineKeyboardMarkup(keyboardRow)
	// --------------------------------

	// Обрабатываем обновления
	for update := range updates {

		// 1. ОБРАБОТКА CALLBACK QUERY (НАЖАТИЕ INLINE-КНОПКИ)
		if update.CallbackQuery != nil {
			callback := update.CallbackQuery

			// Отправляем ответ на запрос (убирает "часики" с кнопки)
			callbackConfig := tgbotapi.NewCallback(callback.ID, "")
			if _, err := bot.Request(callbackConfig); err != nil {
				log.Println(err)
			}

			// Логика кнопки "Показать мой ID"
			if callback.Data == "show_my_id" {
				userID := callback.From.ID
				text := fmt.Sprintf("Твой ID: %d", userID)

				// Отправляем сообщение в чат
				// ВАЖНО: Chat.ID берется из сообщения, к которому была прикреплена кнопка.
				msg := tgbotapi.NewMessage(callback.Message.Chat.ID, text)
				if _, err := bot.Send(msg); err != nil {
					log.Println(err)
				}
			}
			continue // Запрос Callback обработан, переходим к следующему обновлению
		}

		// 2. ОБРАБОТКА ОБЫЧНЫХ СООБЩЕНИЙ (ТЕКСТ/КОМАНДЫ)
		if update.Message != nil {

			// Если это команда
			if update.Message.IsCommand() {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, "")
				switch update.Message.Command() {
				case "start":
					msg.Text = "Привет! Я бот на GoLang с кнопками."
					msg.ReplyMarkup = inlineKeyboard // Прикрепляем кнопки
				case "info":
					// Логика /info без изменений
					response := fmt.Sprintf(
						"Ваша информация:\nID: %d\nИмя: %s\nЮзернейм: @%s",
						update.Message.From.ID, update.Message.From.FirstName, update.Message.From.UserName)
					msg.Text = response
				default:
					msg.Text = "Неизвестная команда."
				}

				if _, err := bot.Send(msg); err != nil {
					log.Println(err)
				}
				continue // Команда обработана
			}

			// 3. ЛОГИКА "ЭХО" (для не-командного текста)
			log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, update.Message.Text)
			if _, err := bot.Send(msg); err != nil {
				log.Println(err)
			}
		}
	}
}
