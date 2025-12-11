package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// !!! ЗАМЕНИТЕ ЭТОТ ID НА ID ВАШЕЙ ТАБЛИЦЫ !!!
const spreadsheetID = "12d036WzCPyL97CtbiU2Vx2BQtr2JDDpVx9mBwSTmwo8"
const leaderboardSheet = "Leaderboard"
const leaderboardRange = "A2:D" // Теперь используется
const writeRangeHtoK = "H:K"    // Диапазон для записи результатов в тесте
const readRangeH2toK = "H2:K"   // Диапазон для чтения результатов в тесте (пропуская H1)
const readRangeA2toF = "A2:F"   // Диапазон для чтения вопросов в тесте (пропуская A1)

// --- ГЛОБАЛЬНЫЕ ПЕРЕМЕННЫЕ ДЛЯ ДОСТУПА К API ---
var sheetsService *sheets.Service
var botAPI *tgbotapi.BotAPI
var leaderboardMutex sync.Mutex

// --- ГЛОБАЛЬНЫЕ СТРУКТУРЫ ДЛЯ ТЕСТОВ ---

// Структура для хранения одного вопроса теста
type TestQuestion struct {
	ID            string
	Question      string
	Options       []string
	CorrectAnswer int
}

// Структура для агрегации статистики пользователя
type UserStats struct {
	Username    string
	UserID      string
	TotalScore  int
	TotalPassed int
}

// Глобальная переменная для хранения текущего загруженного теста
var currentTest []TestQuestion
var currentTestName string

// Глобальная переменная для отслеживания состояния пользователя
var userState = make(map[int64]int)
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

	// --- ЗАПУСК ФОНОВОГО ОБНОВЛЕНИЯ LEADERBOARD ---
	go startLeaderboardUpdater()
	// ------------------------------------------------

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

						editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, fmt.Sprintf("Вы ответили на вопрос %d. Загружаю следующий...", qIndex+1))
						editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
						botAPI.Send(editMsg)

						sendQuestion(botAPI, sheetsService, chatID, userID, userName)
					}
				}

				// --- ОБРАБОТКА ВЫБОРА ТЕСТА (нажатие кнопки "Тесты") ---
			} else if callbackData == "start_tests" {
				// 🟢 БЛОК: Показ списка доступных тестов

				testNames, err := getTestNames()
				if err != nil {
					log.Printf("Ошибка при получении названий тестов: %v", err)
					text := "Не удалось загрузить список тестов. Проверьте настройки таблицы."
					botAPI.Send(tgbotapi.NewMessage(chatID, text))
				} else if len(testNames) == 0 {
					text := "Тесты не найдены. Создайте вкладки для тестов."
					botAPI.Send(tgbotapi.NewMessage(chatID, text))
				} else {
					var testButtons [][]tgbotapi.InlineKeyboardButton
					for _, name := range testNames {
						btn := tgbotapi.NewInlineKeyboardButtonData(name, "select_"+name)
						testButtons = append(testButtons, tgbotapi.NewInlineKeyboardRow(btn))
					}

					backButton := tgbotapi.NewInlineKeyboardButtonData("⏪ Назад", "show_start_menu")
					testButtons = append(testButtons, tgbotapi.NewInlineKeyboardRow(backButton))

					keyboard := tgbotapi.NewInlineKeyboardMarkup(testButtons...)

					editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, "✅ Доступные тесты:")
					editMsg.ReplyMarkup = &keyboard
					botAPI.Send(editMsg)
				}

				// --- ОБРАБОТКА ВЫБОРА КОНКРЕТНОГО ТЕСТА (select_ИмяВкладки) ---
			} else if strings.HasPrefix(callbackData, "select_") {
				testName := strings.TrimPrefix(callbackData, "select_")
				log.Printf("Пользователь [%s] выбрал тест: %s", callback.From.UserName, testName)

				// 1. Загрузка выбранного теста
				var errLoad error
				currentTest, errLoad = loadTestFromSheets(sheetsService, spreadsheetID, testName)
				if errLoad != nil {
					log.Printf("Ошибка при загрузке теста %s: %v", testName, errLoad)
					text := fmt.Sprintf("Ошибка загрузки вопросов из вкладки %s. Убедитесь, что данные начинаются с A2.", testName)
					botAPI.Send(tgbotapi.NewMessage(chatID, text))
					return
				}
				currentTestName = testName

				// 2. Инициализация и старт теста
				userState[userID] = 0
				userScores[userID] = 0

				userName := callback.From.UserName
				if userName == "" {
					userName = fmt.Sprintf("ID_%d", userID)
				}

				deleteMsg := tgbotapi.NewDeleteMessage(chatID, callback.Message.MessageID)
				botAPI.Send(deleteMsg)

				sendQuestion(botAPI, sheetsService, chatID, userID, userName)

				// --- ОБРАБОТКА ЛИЧНОГО КАБИНЕТА (ЧТЕНИЕ ИЗ LEADERBOARD) ---
			} else if callbackData == "show_lk" {
				stats, err := getUserStatsFromLeaderboard(userID)
				if err != nil {
					log.Println("Ошибка получения статистики из Leaderboard:", err)
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

				scoreText := fmt.Sprintf("%d (по %d тестам)", stats.TotalScore, stats.TotalPassed)
				if stats.TotalPassed == 0 {
					scoreText = "Нет пройденных тестов"
				}

				response := fmt.Sprintf(
					"📊 *Личный Кабинет*\n"+
						"Имя/Фамилия: %s\n"+
						"Общий балл: %s\n"+
						"Пройдено уникальных тестов: %d",
					fullName,
					scoreText,
					stats.TotalPassed,
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

			callbackConfig := tgbotapi.NewCallback(callback.ID, "Запрос обработан!")
			botAPI.Request(callbackConfig)

			continue
		}

		// 2. ОБРАБОТКА ОБЫЧНЫХ СООБЩЕНИЙ (ТЕКСТ/КОМАНДЫ)
		if update.Message != nil {
			log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)

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
					msg.Text = "Выберите кнопку 'Тесты', чтобы увидеть список доступных викторин."
					msg.ReplyMarkup = inlineKeyboard
				default:
					msg.Text = "Неизвестная команда."
				}

				if _, err := botAPI.Send(msg); err != nil {
					log.Println(err)
				}
				continue
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

// startLeaderboardUpdater запускает фоновый процесс обновления Leaderboard каждые 5 минут
func startLeaderboardUpdater() {
	if err := updateLeaderboard(); err != nil {
		log.Printf("Ошибка при стартовом обновлении Leaderboard: %v", err)
	} else {
		log.Println("Leaderboard успешно обновлен при старте.")
	}

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		if err := updateLeaderboard(); err != nil {
			log.Printf("Ошибка при фоновом обновлении Leaderboard: %v", err)
		} else {
			log.Println("Leaderboard успешно обновлен.")
		}
	}
}

// updateLeaderboard агрегирует лучший результат каждого пользователя по всем тестам и записывает в Leaderboard.
func updateLeaderboard() error {
	leaderboardMutex.Lock()
	defer leaderboardMutex.Unlock()

	ctx := context.Background()

	allSheets, err := sheetsService.Spreadsheets.Get(spreadsheetID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("не удалось получить свойства таблицы для Leaderboard: %w", err)
	}

	userBestScores := make(map[string]map[string]int)
	userNames := make(map[string]string)

	// 2. Проходим по всем вкладкам, ища вкладки с тестами
	for _, sheet := range allSheets.Sheets {
		sheetTitle := sheet.Properties.Title
		sheetTitleLower := strings.ToLower(sheetTitle)

		// Фильтруем служебные вкладки
		if strings.Contains(sheetTitleLower, "leaderboard") || strings.Contains(sheetTitleLower, "results") {
			continue
		}

		// Диапазон: H2:K (UserID, Username, Score, Timestamp)
		readRange := fmt.Sprintf("%s!%s", sheetTitle, readRangeH2toK)

		resp, err := sheetsService.Spreadsheets.Values.Get(spreadsheetID, readRange).Context(ctx).Do()
		if err != nil {
			log.Printf("Предупреждение: Не удалось прочитать результаты H2:K из вкладки %s: %v", sheetTitle, err)
			continue
		}

		// 3. Собираем лучший результат каждого пользователя в этом тесте
		testName := sheetTitle

		for _, row := range resp.Values {
			if len(row) < 3 {
				continue
			}

			// Колонки: H (индекс 0), I (индекс 1), J (индекс 2)
			userIDStr := row[0].(string)
			username := row[1].(string)
			scoreStr := row[2].(string)

			scoreParts := strings.Split(scoreStr, "/")
			if len(scoreParts) != 2 {
				continue
			}
			score, err := strconv.Atoi(scoreParts[0])
			if err != nil {
				continue
			}

			userNames[userIDStr] = username

			if _, ok := userBestScores[userIDStr]; !ok {
				userBestScores[userIDStr] = make(map[string]int)
			}

			if score > userBestScores[userIDStr][testName] {
				userBestScores[userIDStr][testName] = score
			}
		}
	}

	// 4. Агрегация: Суммируем баллы и считаем уникальные тесты
	var aggregatedStats []UserStats
	for userIDStr, scoresByTest := range userBestScores {
		totalScore := 0
		totalPassed := 0

		for _, score := range scoresByTest {
			totalScore += score
			totalPassed++
		}

		aggregatedStats = append(aggregatedStats, UserStats{
			UserID:      userIDStr,
			Username:    userNames[userIDStr],
			TotalScore:  totalScore,
			TotalPassed: totalPassed,
		})
	}

	// 5. Ранжирование по TotalScore (по убыванию)
	sort.Slice(aggregatedStats, func(i, j int) bool {
		if aggregatedStats[i].TotalScore != aggregatedStats[j].TotalScore {
			return aggregatedStats[i].TotalScore > aggregatedStats[j].TotalScore
		}
		if aggregatedStats[i].TotalPassed != aggregatedStats[j].TotalPassed {
			return aggregatedStats[i].TotalPassed > aggregatedStats[j].TotalPassed
		}
		return aggregatedStats[i].Username < aggregatedStats[j].Username
	})

	// 6. Форматирование для записи
	var values [][]interface{}
	for _, stat := range aggregatedStats {
		values = append(values, []interface{}{
			stat.UserID,
			stat.Username,
			stat.TotalScore,
			stat.TotalPassed,
		})
	}

	// 7. Очистка и запись в Leaderboard
	clearRange := fmt.Sprintf("%s!%s", leaderboardSheet, leaderboardRange) // Использование константы leaderboardRange
	clearRequest := &sheets.ClearValuesRequest{}
	sheetsService.Spreadsheets.Values.Clear(spreadsheetID, clearRange, clearRequest).Context(ctx).Do()

	if len(values) > 0 {
		valueRange := &sheets.ValueRange{
			Values: values,
		}

		writeRange := fmt.Sprintf("%s!%s", leaderboardSheet, leaderboardRange) // Использование константы leaderboardRange
		_, err = sheetsService.Spreadsheets.Values.Update(spreadsheetID, writeRange, valueRange).
			ValueInputOption("USER_ENTERED").
			Context(ctx).
			Do()

		if err != nil {
			return fmt.Errorf("ошибка записи в Leaderboard: %w", err)
		}
	}

	return nil
}

// getUserStatsFromLeaderboard считывает статистику пользователя из Leaderboard.
func getUserStatsFromLeaderboard(userID int64) (UserStats, error) {
	leaderboardMutex.Lock()
	defer leaderboardMutex.Unlock()
	ctx := context.Background()
	stats := UserStats{TotalPassed: 0, TotalScore: 0}

	// Читаем Leaderboard (A: UserID, B: Username, C: Score, D: Passed)
	readRange := fmt.Sprintf("%s!%s", leaderboardSheet, leaderboardRange) // Использование константы leaderboardRange
	resp, err := sheetsService.Spreadsheets.Values.Get(spreadsheetID, readRange).Context(ctx).Do()
	if err != nil {
		return stats, fmt.Errorf("ошибка чтения Leaderboard: %w", err)
	}

	userIDStr := fmt.Sprintf("%d", userID)

	if len(resp.Values) == 0 {
		return stats, nil
	}

	// Ищем пользователя по UserID в колонке A (индекс 0)
	for _, row := range resp.Values {
		if len(row) >= 4 && row[0].(string) == userIDStr {
			stats.UserID = row[0].(string)
			stats.Username = row[1].(string)

			// Score is in C (index 2)
			if score, err := strconv.Atoi(row[2].(string)); err == nil {
				stats.TotalScore = score
			}
			// Passed is in D (index 3)
			if passed, err := strconv.Atoi(row[3].(string)); err == nil {
				stats.TotalPassed = passed
			}
			return stats, nil
		}
	}

	return stats, nil
}

// loadTestFromSheets считывает вопросы и ответы из указанной вкладки (sheetName)
func loadTestFromSheets(service *sheets.Service, spreadsheetID string, sheetName string) ([]TestQuestion, error) {
	// Читаем вопросы из диапазона A2:F
	readRange := fmt.Sprintf("%s!%s", sheetName, readRangeA2toF) // Использование константы readRangeA2toF
	ctx := context.Background()

	resp, err := service.Spreadsheets.Values.Get(spreadsheetID, readRange).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("ошибка получения данных из Sheets (%s): %w", sheetName, err)
	}

	if len(resp.Values) == 0 {
		return nil, fmt.Errorf("во вкладке %s не найдено вопросов в диапазоне A2:F", sheetName)
	}

	var testData []TestQuestion
	for _, row := range resp.Values {
		if len(row) < 6 {
			log.Printf("В строке не хватает данных или не все опции заполнены: %v", row)
			continue
		}

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

	resp, err := sheetsService.Spreadsheets.Get(spreadsheetID).Context(ctx).Fields("sheets.properties.title").Do()
	if err != nil {
		return nil, fmt.Errorf("не удалось получить свойства таблицы: %v", err)
	}

	var testTitles []string
	for _, sheet := range resp.Sheets {
		title := sheet.Properties.Title

		titleLower := strings.ToLower(title)

		// 🚨 ФИЛЬТР: Исключаем вкладки, содержащие "leaderboard" или "results".
		if strings.Contains(titleLower, "leaderboard") || strings.Contains(titleLower, "results") {
			continue
		}

		testTitles = append(testTitles, title)
	}
	return testTitles, nil
}

// sendQuestion отправляет текущий вопрос пользователю
func sendQuestion(bot *tgbotapi.BotAPI, service *sheets.Service, chatID int64, userID int64, username string) {
	qIndex := userState[userID]

	if qIndex >= len(currentTest) {
		currentScore := userScores[userID]
		totalQuestions := len(currentTest)

		err := writeResultToSheets(service, userID, username, currentScore, totalQuestions, currentTestName)

		if err != nil {
			log.Println("Ошибка записи результата:", err)
		}

		finalText := fmt.Sprintf("Тест завершен!\nВаш результат: %d из %d.", currentScore, totalQuestions)

		if err == nil {
			finalText += "\nРезультат сохранен и обновлен."
		}

		// Запускаем асинхронное обновление Leaderboard
		go func() {
			if err := updateLeaderboard(); err != nil {
				log.Printf("Ошибка при обновлении Leaderboard после теста: %v", err)
			}
		}()

		// --- НОВАЯ КЛАВИАТУРА ПОСЛЕ ТЕСТА ---
		buttonLK := tgbotapi.NewInlineKeyboardButtonData("Личный Кабинет (ЛК)", "show_lk")
		buttonTests := tgbotapi.NewInlineKeyboardButtonData("К Списку тестов", "start_tests")

		keyboardRow := tgbotapi.NewInlineKeyboardRow(buttonTests, buttonLK)
		postTestKeyboard := tgbotapi.NewInlineKeyboardMarkup(keyboardRow)
		// ------------------------------------

		finalMsg := tgbotapi.NewMessage(chatID, finalText)
		finalMsg.ReplyMarkup = postTestKeyboard // Прикрепляем новую клавиатуру
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

// writeResultToSheets ищет предыдущий лучший результат пользователя в той же вкладке и обновляет его.
func writeResultToSheets(service *sheets.Service, userID int64, username string, currentScore int, totalQuestions int, testName string) error {
	ctx := context.Background()

	resultSheetName := testName
	// Диапазон чтения: H2:K
	readRange := fmt.Sprintf("%s!%s", resultSheetName, readRangeH2toK)
	// Диапазон записи: H:K
	writeRange := fmt.Sprintf("%s!%s", resultSheetName, writeRangeHtoK)

	resp, err := service.Spreadsheets.Values.Get(spreadsheetID, readRange).Context(ctx).Do()
	if err != nil {
		log.Printf("Предупреждение: Не удалось прочитать результаты из %s. Будет предпринята попытка записи новой строки. Ошибка: %v", resultSheetName, err)
	}

	var updateCellRange string
	var previousBestScore int

	if resp != nil && len(resp.Values) > 0 {
		for i, row := range resp.Values {
			if len(row) > 0 && row[0] == fmt.Sprintf("%d", userID) {
				foundRowIndex := i + 2

				if len(row) > 2 {
					scoreParts := strings.Split(row[2].(string), "/")
					if len(scoreParts) == 2 {
						if score, err := strconv.Atoi(scoreParts[0]); err == nil {
							previousBestScore = score
						}
					}
				}

				if currentScore <= previousBestScore {
					log.Printf("Результат пользователя %d (%d) в тесте %s не лучше предыдущего (%d). Пропуск записи.", userID, currentScore, testName, previousBestScore)
					return nil
				}

				updateCellRange = fmt.Sprintf("%s!H%d", resultSheetName, foundRowIndex)
				break
			}
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

	if updateCellRange != "" {
		_, err = service.Spreadsheets.Values.Update(spreadsheetID, updateCellRange, valueRange).
			ValueInputOption("USER_ENTERED").
			Context(ctx).
			Do()
		log.Printf("Обновлен лучший результат для пользователя %d в тесте %s: %s", userID, testName, newScoreText)

	} else {
		_, err = service.Spreadsheets.Values.Append(spreadsheetID, writeRange, valueRange).
			ValueInputOption("USER_ENTERED").
			InsertDataOption("INSERT_ROWS").
			Context(ctx).
			Do()
		log.Printf("Записан новый результат для пользователя %d в тесте %s: %s", userID, testName, newScoreText)
	}

	if err != nil {
		return fmt.Errorf("ошибка записи/обновления результатов в %s: %w", resultSheetName, err)
	}

	return nil
}
