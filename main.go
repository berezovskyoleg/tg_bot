package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// !!! ЗАМЕНИТЕ ЭТОТ ID НА ID ВАШЕЙ ТАБЛИЦЫ !!!
const spreadsheetID = "12d036WzCPyL97CtbiU2Vx2BQtr2JDDpVx9mBwSTmwo8"
const sheetRange = "Results1!A:D" // Диапазон для записи результатов

// --- ГЛОБАЛЬНЫЕ СТРУКТУРЫ ДЛЯ ТЕСТОВ ---

// Структура для хранения одного вопроса теста
type TestQuestion struct {
	ID            string
	Question      string
	Options       []string
	CorrectAnswer int // Индекс правильного ответа (1, 2, 3...)
}

// Глобальная переменная для хранения всех тестов
var currentTest []TestQuestion

// Глобальная переменная для отслеживания состояния пользователя
// [UserID]CurrentQuestionIndex
var userState = make(map[int64]int)

// [UserID]Score
var userScores = make(map[int64]int)

// --- ОСНОВНАЯ ФУНКЦИЯ ---

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

	// --- ИНИЦИАЛИЗАЦИЯ GOOGLE SHEETS API ---
	ctx := context.Background()

	// Аутентификация с помощью JSON-ключа
	data, err := os.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("Не удалось прочитать JSON-ключ: %v", err)
	}

	conf, err := google.JWTConfigFromJSON(data, sheets.SpreadsheetsScope)
	if err != nil {
		log.Fatalf("Не удалось создать конфигурацию JWT: %v", err)
	}

	client := conf.Client(ctx)
	sheetsService, err := sheets.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Не удалось создать клиент Sheets API: %v", err)
	}
	log.Println("Клиент Google Sheets API успешно инициализирован.")
	// ----------------------------------------

	// --- ЗАГРУЗКА ТЕСТА ИЗ GOOGLE SHEETS ---
	var errLoad error
	currentTest, errLoad = loadTestFromSheets(sheetsService, spreadsheetID)
	if errLoad != nil {
		log.Fatalf("Критическая ошибка при загрузке теста: %v", errLoad)
	}
	log.Printf("Успешно загружено %d вопросов из таблицы.", len(currentTest))
	// ----------------------------------------

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	// --- ИНИЦИАЛИЗАЦИЯ INLINE-КЛАВИАТУРЫ ---
	buttonID := tgbotapi.NewInlineKeyboardButtonData("Показать мой ID", "show_my_id")
	buttonGo := tgbotapi.NewInlineKeyboardButtonURL("Сайт Go", "https://golang.org/")
	keyboardRow := tgbotapi.NewInlineKeyboardRow(buttonID, buttonGo)
	inlineKeyboard := tgbotapi.NewInlineKeyboardMarkup(keyboardRow)
	// ---------------------------------------

	// Обрабатываем обновления
	for update := range updates {

		// 1. ОБРАБОТКА CALLBACK QUERY (НАЖАТИЕ INLINE-КНОПКИ)
		if update.CallbackQuery != nil {
			callback := update.CallbackQuery
			callbackData := callback.Data

			log.Printf("Получен Callback от [%s]: %s", callback.From.UserName, callbackData)

			// Если это ответ на тест
			if strings.HasPrefix(callbackData, "answer_") {

				// Проверка, что пользователь начал тест
				if _, exists := userState[callback.From.ID]; exists {
					// Парсим данные: answer_<индекс вопроса>|<индекс ответа>
					parts := strings.Split(callbackData, "|")
					if len(parts) == 2 {
						// AnswerIndex - это выбранный пользователем ответ (1, 2, или 3)
						answerIndex, _ := strconv.Atoi(parts[1])
						qIndex := userState[callback.From.ID]

						// Логика проверки ответа (см. loadTestFromSheets)
						if qIndex < len(currentTest) && answerIndex == currentTest[qIndex].CorrectAnswer {
							// Если ответ правильный, увеличиваем счет
							userScores[callback.From.ID]++
							log.Printf("Пользователь [%s] ответил верно!", callback.From.UserName)
						} else {
							log.Printf("Пользователь [%s] ответил неверно.", callback.From.UserName)
						}

						// 2. Увеличиваем индекс вопроса
						userState[callback.From.ID]++

						// Редактируем сообщение, чтобы убрать кнопки с предыдущего вопроса
						editMsg := tgbotapi.NewEditMessageText(
							callback.Message.Chat.ID,
							callback.Message.MessageID,
							fmt.Sprintf("Вы ответили на вопрос %d. Загружаю следующий...", qIndex+1),
						)
						editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}} // Убираем кнопки
						bot.Send(editMsg)

						// Отправляем следующий вопрос или завершаем тест
						sendQuestion(bot, sheetsService, callback.Message.Chat.ID, callback.From.ID)
					}
				}
			} else if callbackData == "show_my_id" {
				userID := callback.From.ID
				text := fmt.Sprintf("Твой ID: %d", userID)
				msg := tgbotapi.NewMessage(callback.Message.Chat.ID, text)
				if _, err := bot.Send(msg); err != nil {
					log.Println(err)
				}
			}

			// Отправляем ответ на запрос (убирает "часики")
			callbackConfig := tgbotapi.NewCallback(callback.ID, "Ответ принят!")
			bot.Request(callbackConfig)

			continue
		}

		// 2. ОБРАБОТКА ОБЫЧНЫХ СООБЩЕНИЙ (ТЕКСТ/КОМАНДЫ)
		if update.Message != nil {
			log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)

			// Если это команда
			if update.Message.IsCommand() {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, "")
				switch update.Message.Command() {
				case "start":
					msg.Text = "Привет! Я бот на GoLang. Используй /tests для начала викторины."
					msg.ReplyMarkup = inlineKeyboard
				case "info":
					response := fmt.Sprintf(
						"Ваша информация:\nID: %d\nИмя: %s\nЮзернейм: @%s",
						update.Message.From.ID, update.Message.From.FirstName, update.Message.From.UserName)
					msg.Text = response
				case "tests":
					if len(currentTest) == 0 {
						msg.Text = "Тест недоступен. Проверьте логи на ошибки загрузки."
					} else {
						// Сбрасываем состояние и счет и начинаем с 0-го вопроса
						userState[update.Message.From.ID] = 0
						userScores[update.Message.From.ID] = 0
						sendQuestion(bot, sheetsService, update.Message.Chat.ID, update.Message.From.ID)
						continue // Отправка вопроса в sendQuestion, не отправляем тут msg
					}
				default:
					msg.Text = "Неизвестная команда."
				}

				if _, err := bot.Send(msg); err != nil {
					log.Println(err)
				}
				continue // Команда обработана
			}

			// 3. ЛОГИКА "ЭХО" (для не-командного текста)
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, update.Message.Text)
			if _, err := bot.Send(msg); err != nil {
				log.Println(err)
			}
		}
	}
}

// --- ВСПОМОГАТЕЛЬНЫЕ ФУНКЦИИ ---

// loadTestFromSheets считывает вопросы и ответы из вкладки "Test1"
func loadTestFromSheets(service *sheets.Service, spreadsheetID string) ([]TestQuestion, error) {
	// Читаем диапазон A2:F (со второй строки, чтобы пропустить заголовки)
	readRange := "Test1!A2:F"

	resp, err := service.Spreadsheets.Values.Get(spreadsheetID, readRange).Do()
	if err != nil {
		return nil, fmt.Errorf("ошибка получения данных из Sheets: %w", err)
	}

	if len(resp.Values) == 0 {
		return nil, fmt.Errorf("в таблице Test1 не найдено данных")
	}

	var testData []TestQuestion
	// Проходим по каждой строке (каждому вопросу)
	for _, row := range resp.Values {
		// Мы ожидаем 6 столбцов (A, B, C, D, E, F)
		if len(row) < 6 {
			log.Printf("В строке не хватает данных или не все опции заполнены: %v", row)
			continue
		}

		// Преобразуем строковое значение правильного ответа в число (Column F)
		// ВАЖНО: правильный ответ должен быть числом (1, 2 или 3)
		correct, err := strconv.Atoi(row[5].(string))
		if err != nil || correct < 1 || correct > 3 {
			log.Printf("Неверный формат правильного ответа (должно быть 1, 2 или 3) в строке %v: %v", row, row[5])
			continue
		}

		question := TestQuestion{
			ID:       row[0].(string),
			Question: row[1].(string),
			Options: []string{
				row[2].(string), // Option 1 (Column C)
				row[3].(string), // Option 2 (Column D)
				row[4].(string), // Option 3 (Column E)
			},
			CorrectAnswer: correct,
		}
		testData = append(testData, question)
	}

	return testData, nil
}

// sendQuestion отправляет текущий вопрос пользователю
func sendQuestion(bot *tgbotapi.BotAPI, service *sheets.Service, chatID int64, userID int64) {
	qIndex := userState[userID]

	if qIndex >= len(currentTest) {
		// --- ТЕСТ ЗАВЕРШЕН ---
		currentScore := userScores[userID]
		totalQuestions := len(currentTest)
		username := fmt.Sprintf("@%s", bot.Self.UserName) // Используем юзернейм для записи

		// 1. Запись результата в Sheets
		err := writeResultToSheets(service, userID, username, currentScore, totalQuestions)
		if err != nil {
			log.Println("Ошибка записи результата:", err)
		}

		// 2. Формирование финального сообщения
		finalText := fmt.Sprintf("Тест завершен!\nВаш результат: %d из %d.", currentScore, totalQuestions)

		// Проверяем, удалось ли найти и записать/обновить результат
		if err == nil {
			// (Здесь можно добавить логику отображения, если результат был улучшен)
			finalText += "\nРезультат сохранен и обновлен."
		}

		finalMsg := tgbotapi.NewMessage(chatID, finalText)
		bot.Send(finalMsg)

		// 3. Очистка состояния
		delete(userState, userID)
		delete(userScores, userID)
		return
	}

	question := currentTest[qIndex]

	// Формируем кнопки-ответы
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, option := range question.Options {
		callbackData := fmt.Sprintf("answer_%d|%d", qIndex, i+1)
		button := tgbotapi.NewInlineKeyboardButtonData(option, callbackData)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(button)) // NewInlineKeyboardRow возвращает []InlineKeyboardButton
	}

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Вопрос %d/%d: %s", qIndex+1, len(currentTest), question.Question))
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)

	if _, err := bot.Send(msg); err != nil {
		log.Println("Ошибка отправки вопроса:", err)
	}
}

// writeResultToSheets ищет предыдущий результат пользователя и перезаписывает, если текущий лучше
func writeResultToSheets(service *sheets.Service, userID int64, username string, currentScore int, totalQuestions int) error {
	ctx := context.Background()

	// 1. Читаем все существующие результаты, чтобы найти предыдущий
	readRange := "Results1!A:D" // A: UserID, B: Username, C: Score, D: Timestamp
	resp, err := service.Spreadsheets.Values.Get(spreadsheetID, readRange).Do()
	if err != nil {
		return fmt.Errorf("ошибка чтения результатов: %w", err)
	}

	var updateRange string
	var previousBestScore int

	// Ищем строку, принадлежащую текущему пользователю
	for i, row := range resp.Values {
		// Пропускаем заголовок
		if i == 0 {
			continue
		}

		// Ожидаем, что UserID находится в первой колонке (row[0])
		if len(row) > 0 && row[0] == fmt.Sprintf("%d", userID) {
			// Строка найдена. Проверяем предыдущий балл.
			if len(row) > 2 {
				// Пытаемся распарсить предыдущий балл (например, "5/10")
				scoreParts := strings.Split(row[2].(string), "/")
				if len(scoreParts) == 2 {
					if score, err := strconv.Atoi(scoreParts[0]); err == nil {
						previousBestScore = score
					}
				}
			}

			// Если текущий результат не лучше предыдущего, не записываем
			if currentScore <= previousBestScore {
				log.Printf("Результат пользователя %d (%d) не лучше предыдущего (%d). Пропуск записи.", userID, currentScore, previousBestScore)
				return nil // Выходим без записи
			}

			// Если результат лучше, запоминаем диапазон для ОБНОВЛЕНИЯ (i+1, т.к. Sheets использует 1-based indexing)
			updateRange = fmt.Sprintf("Results1!A%d", i+1)
			break
		}
	}

	// 2. Если updateRange найден (результат лучше или это не первая запись)
	//    ИЛИ если это совершенно новый пользователь (updateRange пуст), записываем/обновляем.

	newScoreText := fmt.Sprintf("%d/%d", currentScore, totalQuestions)
	currentTime := time.Now().Format("2006-01-02 15:04:05")

	// Новая строка для записи
	row := []interface{}{
		userID,
		username,
		newScoreText,
		currentTime,
	}

	valueRange := &sheets.ValueRange{
		Values: [][]interface{}{row},
	}

	if updateRange != "" {
		// Обновляем существующую строку с лучшим результатом
		_, err = service.Spreadsheets.Values.Update(spreadsheetID, updateRange, valueRange).
			ValueInputOption("USER_ENTERED").
			Context(ctx).
			Do()
		log.Printf("Обновлен лучший результат для пользователя %d: %s", userID, newScoreText)

	} else {
		// Добавляем новую строку в конец таблицы (для нового пользователя)
		_, err = service.Spreadsheets.Values.Append(spreadsheetID, sheetRange, valueRange).
			ValueInputOption("USER_ENTERED").
			InsertDataOption("INSERT_ROWS").
			Context(ctx).
			Do()
		log.Printf("Записан новый результат для пользователя %d: %s", userID, newScoreText)
	}

	if err != nil {
		return fmt.Errorf("ошибка записи/обновления результатов: %w", err)
	}

	return nil
}
