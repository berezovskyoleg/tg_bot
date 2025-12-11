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

// --- ГЛОБАЛЬНЫЕ ПЕРЕМЕННЫЕ ДЛЯ ДОСТУПА К API ---
var sheetsService *sheets.Service
var botAPI *tgbotapi.BotAPI

// --- ГЛОБАЛЬНЫЕ СТРУКТУРЫ ДЛЯ ТЕСТОВ ---

// Структура для хранения одного вопроса теста
type TestQuestion struct {
	ID            string
	Question      string
	Options       []string
	CorrectAnswer int // Индекс правильного ответа (1, 2, 3...)
}

// Структура для агрегации статистики пользователя
type UserStats struct {
	TotalPassed int
	TotalScore  int
}

// Глобальная переменная для хранения текущего загруженного теста
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

	var err error
	botAPI, err = tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Panic(err)
	}

	log.Printf("Авторизация на аккаунте %s", botAPI.Self.UserName)

	// --- ИНИЦИАЛИЗАЦИЯ GOOGLE SHEETS API (ГЛОБАЛЬНО) ---
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
	sheetsService, err = sheets.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Не удалось создать клиент Sheets API: %v", err)
	}
	log.Println("Клиент Google Sheets API успешно инициализирован.")
	// ----------------------------------------

	// --- ЗАГРУЗКА ТЕСТА ИЗ GOOGLE SHEETS ---
	// Загружаем только Test1 для старта, пока не будет выбора
	var errLoad error
	currentTest, errLoad = loadTestFromSheets(sheetsService, spreadsheetID, "Test1")
	if errLoad != nil {
		log.Printf("Внимание: Критическая ошибка при загрузке Test1 или тест пуст: %v", errLoad)
	} else {
		log.Printf("Успешно загружено %d вопросов из Test1.", len(currentTest))
	}
	// ----------------------------------------

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := botAPI.GetUpdatesChan(u)

	// --- ИНИЦИАЛИЗАЦИЯ INLINE-КЛАВИАТУРЫ ---
	buttonLK := tgbotapi.NewInlineKeyboardButtonData("Личный Кабинет (ЛК)", "show_lk")
	buttonTests := tgbotapi.NewInlineKeyboardButtonData("Тесты", "start_tests")

	keyboardRow := tgbotapi.NewInlineKeyboardRow(buttonLK, buttonTests)
	inlineKeyboard := tgbotapi.NewInlineKeyboardMarkup(keyboardRow)
	// ---------------------------------------

	// Обрабатываем обновления
	for update := range updates {

		// 1. ОБРАБОТКА CALLBACK QUERY (НАЖАТИЕ INLINE-КНОПКИ)
		if update.CallbackQuery != nil {
			callback := update.CallbackQuery
			callbackData := callback.Data
			chatID := callback.Message.Chat.ID
			userID := callback.From.ID

			log.Printf("Получен Callback от [%s]: %s", callback.From.UserName, callbackData)

			// --- ОБРАБОТКА ОТВЕТОВ НА ВОПРОСЫ ---
			if strings.HasPrefix(callbackData, "answer_") {

				if _, exists := userState[userID]; exists {
					userName := callback.From.UserName
					if userName == "" {
						userName = fmt.Sprintf("ID_%d", userID)
					}

					// Парсинг, проверка ответа, увеличение state и score
					parts := strings.Split(callbackData, "|")
					if len(parts) == 2 {
						answerIndex, _ := strconv.Atoi(parts[1])
						qIndex := userState[userID]

						if qIndex < len(currentTest) && answerIndex == currentTest[qIndex].CorrectAnswer {
							userScores[userID]++
							log.Printf("Пользователь [%s] ответил верно!", callback.From.UserName)
						} else {
							log.Printf("Пользователь [%s] ответил неверно.", callback.From.UserName)
						}

						userState[userID]++

						// Редактируем сообщение (убираем кнопки)
						editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, fmt.Sprintf("Вы ответили на вопрос %d. Загружаю следующий...", qIndex+1))
						editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
						botAPI.Send(editMsg)

						// Отправляем следующий вопрос или завершаем тест
						sendQuestion(botAPI, sheetsService, chatID, userID, userName)
					}
				} // Конец if exists

				// --- ОБРАБОТКА ВЫБОРА ТЕСТА ---
			} else if callbackData == "start_tests" {
				// 🟢 НОВЫЙ БЛОК: Показ списка доступных тестов

				testNames, err := getTestNames()
				if err != nil {
					log.Printf("Ошибка при получении названий тестов: %v", err)
					text := "Не удалось загрузить список тестов. Проверьте настройки таблицы."
					botAPI.Send(tgbotapi.NewMessage(chatID, text))
				} else {
					var testButtons [][]tgbotapi.InlineKeyboardButton
					for _, name := range testNames {
						// Используем только вкладки, начинающиеся на "Test"
						if strings.HasPrefix(name, "Test") {
							// Callback data будет "select_Test1"
							btn := tgbotapi.NewInlineKeyboardButtonData(name, "select_"+name)
							testButtons = append(testButtons, tgbotapi.NewInlineKeyboardRow(btn))
						}
					}

					// Кнопка "Назад"
					backButton := tgbotapi.NewInlineKeyboardButtonData("⏪ Назад", "show_start_menu")
					testButtons = append(testButtons, tgbotapi.NewInlineKeyboardRow(backButton))

					keyboard := tgbotapi.NewInlineKeyboardMarkup(testButtons...)

					// Редактируем сообщение для отображения списка тестов
					editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, "✅ Доступные тесты:")
					editMsg.ReplyMarkup = &keyboard
					botAPI.Send(editMsg)
				}

				// --- ОБРАБОТКА ВЫБОРА КОНКРЕТНОГО ТЕСТА (select_Test1) ---
			} else if strings.HasPrefix(callbackData, "select_") {
				testName := strings.TrimPrefix(callbackData, "select_")
				log.Printf("Пользователь [%s] выбрал тест: %s", callback.From.UserName, testName)

				// 1. Загрузка выбранного теста
				var errLoad error
				currentTest, errLoad = loadTestFromSheets(sheetsService, spreadsheetID, testName)
				if errLoad != nil {
					log.Printf("Ошибка при загрузке теста %s: %v", testName, errLoad)
					text := fmt.Sprintf("Ошибка загрузки вопросов из вкладки %s.", testName)
					botAPI.Send(tgbotapi.NewMessage(chatID, text))
					return
				}

				// 2. Инициализация и старт теста
				userState[userID] = 0
				userScores[userID] = 0

				userName := callback.From.UserName
				if userName == "" {
					userName = fmt.Sprintf("ID_%d", userID)
				}

				// Удаляем предыдущее сообщение с выбором
				deleteMsg := tgbotapi.NewDeleteMessage(chatID, callback.Message.MessageID)
				botAPI.Send(deleteMsg)

				// Отправляем первый вопрос
				sendQuestion(botAPI, sheetsService, chatID, userID, userName)

				// --- ОБРАБОТКА ЛИЧНОГО КАБИНЕТА ---
			} else if callbackData == "show_lk" {
				stats, err := getUserStats(sheetsService, userID)
				if err != nil {
					log.Println("Ошибка получения статистики:", err)
					text := "Не удалось загрузить вашу статистику."
					botAPI.Send(tgbotapi.NewMessage(chatID, text))
					return
				}

				fullName := callback.From.FirstName
				if callback.From.LastName != "" {
					fullName += " " + callback.From.LastName
				} else if fullName == "" {
					fullName = fmt.Sprintf("ID: %d", userID)
				}

				response := fmt.Sprintf(
					"📊 *Личный Кабинет*\n"+
						"Имя/Фамилия: %s\n"+
						"Пройдено тестов: %d\n"+
						"Общий балл: %d",
					fullName,
					stats.TotalPassed,
					stats.TotalScore,
				)

				msg := tgbotapi.NewMessage(chatID, response)
				msg.ParseMode = tgbotapi.ModeMarkdown
				botAPI.Send(msg)

				// --- ОБРАБОТКА КНОПКИ НАЗАД ---
			} else if callbackData == "show_start_menu" {
				editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, "Привет! Выберите действие:")
				editMsg.ReplyMarkup = &inlineKeyboard
				botAPI.Send(editMsg)
			}

			// Отправляем ответ на запрос (убирает "часики")
			callbackConfig := tgbotapi.NewCallback(callback.ID, "Запрос обработан!")
			botAPI.Request(callbackConfig)

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
					msg.Text = "Привет! Я бот на GoLang. Выберите действие."
					msg.ReplyMarkup = inlineKeyboard
				case "info":
					response := fmt.Sprintf(
						"Ваша информация:\nID: %d\nИмя: %s\nЮзернейм: @%s",
						update.Message.From.ID, update.Message.From.FirstName, update.Message.From.UserName)
					msg.Text = response
				case "tests":
					// Теперь команда /tests отправляет то же сообщение, что и /start,
					// чтобы пользователь нажал кнопку "Тесты" и увидел список.
					msg.Text = "Выберите кнопку 'Тесты', чтобы увидеть список доступных викторин."
					msg.ReplyMarkup = inlineKeyboard
				default:
					msg.Text = "Неизвестная команда."
				}

				if _, err := botAPI.Send(msg); err != nil {
					log.Println(err)
				}
				continue // Команда обработана
			}

			// 3. ЛОГИКА "ЭХО"
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, update.Message.Text)
			if _, err := botAPI.Send(msg); err != nil {
				log.Println(err)
			}
		}
	}
}

// --- ВСПОМОГАТЕЛЬНЫЕ ФУНКЦИИ ---

// loadTestFromSheets считывает вопросы и ответы из указанной вкладки (sheetName)
func loadTestFromSheets(service *sheets.Service, spreadsheetID string, sheetName string) ([]TestQuestion, error) {
	// Читаем диапазон A2:F (со второй строки, чтобы пропустить заголовки)
	readRange := fmt.Sprintf("%s!A2:F", sheetName)

	resp, err := service.Spreadsheets.Values.Get(spreadsheetID, readRange).Context(context.Background()).Do()
	if err != nil {
		return nil, fmt.Errorf("ошибка получения данных из Sheets (%s): %w", sheetName, err)
	}

	if len(resp.Values) == 0 {
		return nil, fmt.Errorf("во вкладке %s не найдено данных", sheetName)
	}

	var testData []TestQuestion
	for _, row := range resp.Values {
		if len(row) < 6 {
			log.Printf("В строке не хватает данных или не все опции заполнены: %v", row)
			continue
		}

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
				row[2].(string),
				row[3].(string),
				row[4].(string),
			},
			CorrectAnswer: correct,
		}
		testData = append(testData, question)
	}

	return testData, nil
}

// getTestNames извлекает названия всех вкладок (листов) из таблицы.
func getTestNames() ([]string, error) {
	ctx := context.Background()

	// Запрашиваем только названия листов
	resp, err := sheetsService.Spreadsheets.Get(spreadsheetID).Context(ctx).Fields("sheets.properties.title").Do()
	if err != nil {
		return nil, fmt.Errorf("не удалось получить свойства таблицы: %v", err)
	}

	var sheetTitles []string
	for _, sheet := range resp.Sheets {
		sheetTitles = append(sheetTitles, sheet.Properties.Title)
	}
	return sheetTitles, nil
}

// getUserStats считывает результаты пользователя из Sheets и агрегирует статистику.
func getUserStats(service *sheets.Service, userID int64) (UserStats, error) {
	ctx := context.Background()
	stats := UserStats{}

	// Читаем только из Results1
	readRange := "Results1!A:C"
	resp, err := service.Spreadsheets.Values.Get(spreadsheetID, readRange).Context(ctx).Do()
	if err != nil {
		return stats, fmt.Errorf("ошибка чтения результатов для статистики: %w", err)
	}
	// ... (остальной код getUserStats без изменений)

	if len(resp.Values) <= 1 {
		return stats, nil
	}

	for i, row := range resp.Values {
		if i == 0 {
			continue
		}

		if len(row) < 3 {
			continue
		}

		sheetUserID := row[0].(string)

		if sheetUserID == fmt.Sprintf("%d", userID) {
			stats.TotalPassed++

			scoreText := row[2].(string)
			scoreParts := strings.Split(scoreText, "/")
			if len(scoreParts) == 2 {
				if score, err := strconv.Atoi(scoreParts[0]); err == nil {
					stats.TotalScore += score
				}
			}
		}
	}

	return stats, nil
}

// sendQuestion отправляет текущий вопрос пользователю
func sendQuestion(bot *tgbotapi.BotAPI, service *sheets.Service, chatID int64, userID int64, username string) {
	// ... (код sendQuestion без изменений)
	qIndex := userState[userID]

	if qIndex >= len(currentTest) {
		currentScore := userScores[userID]
		totalQuestions := len(currentTest)

		err := writeResultToSheets(service, userID, username, currentScore, totalQuestions)

		if err != nil {
			log.Println("Ошибка записи результата:", err)
		}

		finalText := fmt.Sprintf("Тест завершен!\nВаш результат: %d из %d.", currentScore, totalQuestions)

		if err == nil {
			finalText += "\nРезультат сохранен и обновлен."
		}

		finalMsg := tgbotapi.NewMessage(chatID, finalText)
		bot.Send(finalMsg)

		delete(userState, userID)
		delete(userScores, userID)
		return
	}

	question := currentTest[qIndex]

	var rows [][]tgbotapi.InlineKeyboardButton
	for i, option := range question.Options {
		callbackData := fmt.Sprintf("answer_%d|%d", qIndex, i+1)
		button := tgbotapi.NewInlineKeyboardButtonData(option, callbackData)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(button))
	}

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Вопрос %d/%d: %s", qIndex+1, len(currentTest), question.Question))
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)

	if _, err := bot.Send(msg); err != nil {
		log.Println("Ошибка отправки вопроса:", err)
	}
}

// writeResultToSheets ищет предыдущий результат пользователя и перезаписывает, если текущий лучше
func writeResultToSheets(service *sheets.Service, userID int64, username string, currentScore int, totalQuestions int) error {
	// ... (код writeResultToSheets без изменений)
	ctx := context.Background()

	readRange := "Results1!A:D"
	resp, err := service.Spreadsheets.Values.Get(spreadsheetID, readRange).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("ошибка чтения результатов: %w", err)
	}

	var updateRange string
	var previousBestScore int

	for i, row := range resp.Values {
		if i == 0 {
			continue
		}

		if len(row) > 0 && row[0] == fmt.Sprintf("%d", userID) {
			if len(row) > 2 {
				scoreParts := strings.Split(row[2].(string), "/")
				if len(scoreParts) == 2 {
					if score, err := strconv.Atoi(scoreParts[0]); err == nil {
						previousBestScore = score
					}
				}
			}

			if currentScore <= previousBestScore {
				log.Printf("Результат пользователя %d (%d) не лучше предыдущего (%d). Пропуск записи.", userID, currentScore, previousBestScore)
				return nil
			}

			updateRange = fmt.Sprintf("Results1!A%d", i+1)
			break
		}
	}

	newScoreText := fmt.Sprintf("%d/%d", currentScore, totalQuestions)
	currentTime := time.Now().Format("2006-01-02 15:04:05")

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
		_, err = service.Spreadsheets.Values.Update(spreadsheetID, updateRange, valueRange).
			ValueInputOption("USER_ENTERED").
			Context(ctx).
			Do()
		log.Printf("Обновлен лучший результат для пользователя %d: %s", userID, newScoreText)

	} else {
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
