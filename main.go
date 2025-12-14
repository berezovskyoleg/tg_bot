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
const spreadsheetID = "12d036WzCPyR97CtbiU2Vx2BQtr2JDDpVx9mBwSTmwo8"

const leaderboardSheet = "Leaderboard"
const teacherSheet = "Teacher"
const leaderboardRange = "A2:D"
const writeRangeHtoK = "H:K"
const readRangeH2toK = "H2:K"
const readRangeA2toF = "A2:F"

// ИСПРАВЛЕННЫЙ ДИАПАЗОН ЧТЕНИЯ для Teacher: A2:A10 (включая пустые строки)
const teacherReadRangeA = "A2:A10"
const teacherReadRangeB = "B2:B12"

// --- ГЛОБАЛЬНЫЕ ПЕРЕМЕННЫЕ ДЛЯ ДОСТУПА К API ---
var sheetsService *sheets.Service
var botAPI *tgbotapi.BotAPI
var leaderboardMutex sync.Mutex

// --- ГЛОБАЛЬНЫЕ СТРУКТУРЫ ДЛЯ ТЕСТОВ ---
type TestQuestion struct {
	ID            string
	Question      string
	Options       []string
	CorrectAnswer int
}

type UserStats struct {
	Username    string
	UserID      string
	TotalScore  int
	TotalPassed int
}

var currentTest []TestQuestion
var currentTestName string

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

	// --- ИНИЦИАЛИЗАЦИЯ GOOGLE SHEETS API ---
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

	// --- ЗАПУСК ФОНОВОГО ОБНОВЛЕНИЯ LEADERBOARD ---
	go startLeaderboardUpdater()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := botAPI.GetUpdatesChan(u)

	// --- ИНИЦИАЛИЗАЦИЯ INLINE-КЛАВИАТУРЫ (ГЛАВНОЕ МЕНЮ) ---
	buttonLK := tgbotapi.NewInlineKeyboardButtonData("ЛК", "show_lk")
	buttonTests := tgbotapi.NewInlineKeyboardButtonData("Тесты", "start_tests")
	buttonTeacher := tgbotapi.NewInlineKeyboardButtonData("Преподаватель", "show_teacher")

	keyboardRow1 := tgbotapi.NewInlineKeyboardRow(buttonTeacher, buttonLK)
	keyboardRow2 := tgbotapi.NewInlineKeyboardRow(buttonTests)
	inlineKeyboard := tgbotapi.NewInlineKeyboardMarkup(keyboardRow1, keyboardRow2)
	// ---------------------------------------

	// =======================================================
	// 🌟 ИНТЕГРИРОВАННАЯ ЛОГИКА ОБРАБОТКИ ОБНОВЛЕНИЙ 🌟
	// =======================================================
	for update := range updates {

		// 1. ОБРАБОТКА МЕДИА (ДЛЯ ПОЛУЧЕНИЯ FILE ID)
		if update.Message != nil {
			chatID := update.Message.Chat.ID
			log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)

			// --- Получение File ID для Фото ---
			if len(update.Message.Photo) > 0 {
				fileID := update.Message.Photo[len(update.Message.Photo)-1].FileID
				log.Printf("📥 File ID для ФОТО: %s", fileID)
				botAPI.Send(tgbotapi.NewMessage(chatID, "Ваш File ID для фото: "+fileID))
				continue
			}

			// --- Получение File ID для Видео ---
			if update.Message.Video != nil {
				fileID := update.Message.Video.FileID
				log.Printf("📥 File ID для ВИДЕО: %s", fileID)
				botAPI.Send(tgbotapi.NewMessage(chatID, "Ваш File ID для видео: "+fileID))
				continue
			}

			// --- Получение File ID для Аудио ---
			if update.Message.Audio != nil {
				fileID := update.Message.Audio.FileID
				log.Printf("📥 File ID для АУДИО: %s", fileID)
				botAPI.Send(tgbotapi.NewMessage(chatID, "Ваш File ID для аудио: "+fileID))
				continue
			}

			// --- ОБРАБОТКА КОМАНД (Если это не медиа) ---
			if update.Message.IsCommand() {
				msg := tgbotapi.NewMessage(chatID, "")
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

			// --- ЛОГИКА "ЭХО" (для обычного текста) ---
			msg := tgbotapi.NewMessage(chatID, update.Message.Text)
			if _, err := botAPI.Send(msg); err != nil {
				log.Println(err)
			}
			continue
		}

		// 2. ОБРАБОТКА CALLBACK QUERY (НАЖАТИЕ INLINE-КНОПКИ)
		if update.CallbackQuery != nil {
			callback := update.CallbackQuery
			callbackData := callback.Data
			chatID := callback.Message.Chat.ID
			userID := callback.From.ID

			log.Printf("Получен Callback от [%s]: %s", callback.From.UserName, callbackData)

			// Удаление "часов" в Telegram после нажатия
			callbackConfig := tgbotapi.NewCallback(callback.ID, "Запрос обработан!")
			botAPI.Request(callbackConfig)

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

				// --- ОБРАБОТКА ВЫБОРА КОНКРЕТНОГО ТЕСТА ---
			} else if strings.HasPrefix(callbackData, "select_") {
				testName := strings.TrimPrefix(callbackData, "select_")

				var errLoad error
				currentTest, errLoad = loadTestFromSheets(sheetsService, spreadsheetID, testName)
				if errLoad != nil {
					log.Printf("Ошибка при загрузке теста %s: %v", testName, errLoad)
					text := fmt.Sprintf("Ошибка загрузки вопросов из вкладки %s.", testName)
					botAPI.Send(tgbotapi.NewMessage(chatID, text))
					return
				}
				currentTestName = testName

				userState[userID] = 0
				userScores[userID] = 0

				userName := callback.From.UserName
				if userName == "" {
					userName = fmt.Sprintf("ID_%d", userID)
				}

				deleteMsg := tgbotapi.NewDeleteMessage(chatID, callback.Message.MessageID)
				botAPI.Send(deleteMsg)

				sendQuestion(botAPI, sheetsService, chatID, userID, userName)

				// --- ОБРАБОТКА ЛИЧНОГО КАБИНЕТА ---
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

				backButton := tgbotapi.NewInlineKeyboardButtonData("⏪ Назад", "show_start_menu")
				keyboard := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(backButton))
				msg.ReplyMarkup = keyboard

				botAPI.Send(msg)

				// --- БЛОК: ИНФОРМАЦИЯ О ПРЕПОДАВАТЕЛЕ (С ЛОГИКОЙ FILE ID/URL) ---
			} else if callbackData == "show_teacher" {

				teacherInfo, err := loadTeacherInfo()
				if err != nil {
					log.Println("Ошибка загрузки данных преподавателя:", err)
					backButton := tgbotapi.NewInlineKeyboardButtonData("⏪ Назад", "show_start_menu")
					keyboard := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(backButton))

					editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, "⚠️ Не удалось загрузить информацию о преподавателе.")
					editMsg.ReplyMarkup = &keyboard
					botAPI.Send(editMsg)
					return
				}

				response := fmt.Sprintf(
					"🧑‍🏫 *%s*\n\n"+
						"%s\n\n"+
						"✉️ Контакты: %s",
					teacherInfo["name"],
					teacherInfo["description"],
					teacherInfo["contacts"],
				)

				backButton := tgbotapi.NewInlineKeyboardButtonData("⏪ Назад", "show_start_menu")
				keyboard := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(backButton))

				lastMsgID := callback.Message.MessageID

				// Удаляем исходное сообщение-кнопку
				deleteMsg := tgbotapi.NewDeleteMessage(chatID, callback.Message.MessageID)
				botAPI.Send(deleteMsg)

				// --- 2. Отправка Фото + Текст (в подписи) ---
				photoSent := false
				if photoIDOrURL, ok := teacherInfo["photo"]; ok && photoIDOrURL != "" {

					// ИСПРАВЛЕНО: Вместо var media tgbotapi.Request; используем конкретный тип
					var photoMsg tgbotapi.PhotoConfig

					if strings.HasPrefix(photoIDOrURL, "http") || strings.HasPrefix(photoIDOrURL, "https") {
						// Это URL
						photoMsg = tgbotapi.NewPhoto(chatID, tgbotapi.FileURL(photoIDOrURL))
					} else {
						// Это File ID
						photoMsg = tgbotapi.NewPhoto(chatID, tgbotapi.FileID(photoIDOrURL))
					}

					// photoMsg := media.(tgbotapi.PhotoConfig) // <-- Эту строку удаляем, она больше не нужна
					photoMsg.Caption = response
					photoMsg.ParseMode = tgbotapi.ModeMarkdown

					if sentMsg, err := botAPI.Send(photoMsg); err == nil {
						photoSent = true
						lastMsgID = sentMsg.MessageID
					} else {
						log.Printf("Не удалось отправить фото преподавателя (ID/URL: %s): %v. Отправка только текста.", photoIDOrURL, err)
					}
				}
				// Если фото не было отправлено, отправляем только текст (новое сообщение)
				if !photoSent {
					newMsg := tgbotapi.NewMessage(chatID, response)
					newMsg.ParseMode = tgbotapi.ModeMarkdown

					if sentMsg, err := botAPI.Send(newMsg); err == nil {
						lastMsgID = sentMsg.MessageID
					}
				}

				// --- 3. Отправка Видео ---
				if videoIDOrURL, ok := teacherInfo["video"]; ok && videoIDOrURL != "" {
					// ИСПРАВЛЕНО: Вместо var media tgbotapi.Request; используем конкретный тип
					var videoMsg tgbotapi.VideoConfig

					if strings.HasPrefix(videoIDOrURL, "http") || strings.HasPrefix(videoIDOrURL, "https") {
						videoMsg = tgbotapi.NewVideo(chatID, tgbotapi.FileURL(videoIDOrURL))
					} else {
						videoMsg = tgbotapi.NewVideo(chatID, tgbotapi.FileID(videoIDOrURL))
					}
					if sentMsg, err := botAPI.Send(videoMsg); err == nil {
						lastMsgID = sentMsg.MessageID
					} else {
						log.Printf("Не удалось отправить видео (ID/URL: %s): %v.", videoIDOrURL, err)
					}
				}

				// --- 4. Отправка Аудио ---
				if audioIDOrURL, ok := teacherInfo["audio"]; ok && audioIDOrURL != "" {
					// ИСПРАВЛЕНО: Вместо var media tgbotapi.Request; используем конкретный тип
					var audioMsg tgbotapi.AudioConfig

					if strings.HasPrefix(audioIDOrURL, "http") || strings.HasPrefix(audioIDOrURL, "https") {
						audioMsg = tgbotapi.NewAudio(chatID, tgbotapi.FileURL(audioIDOrURL))
					} else {
						audioMsg = tgbotapi.NewAudio(chatID, tgbotapi.FileID(audioIDOrURL))
					}
					if sentMsg, err := botAPI.Send(audioMsg); err == nil {
						lastMsgID = sentMsg.MessageID
					} else {
						log.Printf("Не удалось отправить аудио (ID/URL: %s): %v.", audioIDOrURL, err)
					}
				}

				// --- 5. Прикрепляем кнопку "Назад" к последнему отправленному сообщению ---
				if lastMsgID != 0 {
					editMarkup := tgbotapi.NewEditMessageReplyMarkup(chatID, lastMsgID, keyboard)
					botAPI.Send(editMarkup)
				}

				// --- ОБРАБОТКА КНОПКИ НАЗАД (возврат в главное меню) ---
			} else if callbackData == "show_start_menu" {

				msgText := "Привет! Выберите действие:"

				editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, msgText)
				editMsg.ReplyMarkup = &inlineKeyboard

				if _, err := botAPI.Send(editMsg); err != nil {
					newMsg := tgbotapi.NewMessage(chatID, msgText)
					newMsg.ReplyMarkup = inlineKeyboard
					botAPI.Send(newMsg)
				}
			}
		}
	}
}

// --- ВСПОМОГАТЕЛЬНЫЕ ФУНКЦИИ ---

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

	for _, sheet := range allSheets.Sheets {
		sheetTitle := sheet.Properties.Title
		sheetTitleLower := strings.ToLower(sheetTitle)

		if strings.Contains(sheetTitleLower, "leaderboard") || strings.Contains(sheetTitleLower, "results") || sheetTitle == teacherSheet {
			continue
		}

		readRange := fmt.Sprintf("%s!%s", sheetTitle, readRangeH2toK)

		resp, err := sheetsService.Spreadsheets.Values.Get(spreadsheetID, readRange).Context(ctx).Do()
		if err != nil {
			log.Printf("Предупреждение: Не удалось прочитать результаты H2:K из вкладки %s: %v", sheetTitle, err)
			continue
		}

		testName := sheetTitle

		for _, row := range resp.Values {
			if len(row) < 3 {
				continue
			}

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

	sort.Slice(aggregatedStats, func(i, j int) bool {
		if aggregatedStats[i].TotalScore != aggregatedStats[j].TotalScore {
			return aggregatedStats[i].TotalScore > aggregatedStats[j].TotalScore
		}
		if aggregatedStats[i].TotalPassed != aggregatedStats[j].TotalPassed {
			return aggregatedStats[i].TotalPassed > aggregatedStats[j].TotalPassed
		}
		return aggregatedStats[i].Username < aggregatedStats[j].Username
	})

	var values [][]interface{}
	for _, stat := range aggregatedStats {
		values = append(values, []interface{}{
			stat.UserID,
			stat.Username,
			stat.TotalScore,
			stat.TotalPassed,
		})
	}

	clearRange := fmt.Sprintf("%s!%s", leaderboardSheet, leaderboardRange)
	clearRequest := &sheets.ClearValuesRequest{}
	sheetsService.Spreadsheets.Values.Clear(spreadsheetID, clearRange, clearRequest).Context(ctx).Do()

	if len(values) > 0 {
		valueRange := &sheets.ValueRange{
			Values: values,
		}

		writeRange := fmt.Sprintf("%s!%s", leaderboardSheet, leaderboardRange)
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

func getUserStatsFromLeaderboard(userID int64) (UserStats, error) {
	leaderboardMutex.Lock()
	defer leaderboardMutex.Unlock()
	ctx := context.Background()
	stats := UserStats{TotalPassed: 0, TotalScore: 0}

	readRange := fmt.Sprintf("%s!%s", leaderboardSheet, leaderboardRange)
	resp, err := sheetsService.Spreadsheets.Values.Get(spreadsheetID, readRange).Context(ctx).Do()
	if err != nil {
		return stats, fmt.Errorf("ошибка чтения Leaderboard: %w", err)
	}

	userIDStr := fmt.Sprintf("%d", userID)

	if len(resp.Values) == 0 {
		return stats, nil
	}

	for _, row := range resp.Values {
		if len(row) >= 4 && row[0].(string) == userIDStr {
			stats.UserID = row[0].(string)
			stats.Username = row[1].(string)

			if score, err := strconv.Atoi(row[2].(string)); err == nil {
				stats.TotalScore = score
			}
			if passed, err := strconv.Atoi(row[3].(string)); err == nil {
				stats.TotalPassed = passed
			}
			return stats, nil
		}
	}

	return stats, nil
}

func loadTeacherInfo() (map[string]string, error) {
	ctx := context.Background()

	// Читаем колонку A (A2:A10). API вернет 9 строк (индексы 0-8).
	respA, errA := sheetsService.Spreadsheets.Values.Get(spreadsheetID, fmt.Sprintf("%s!%s", teacherSheet, teacherReadRangeA)).Context(ctx).Do()
	if errA != nil {
		return nil, fmt.Errorf("ошибка получения данных из Sheets (A): %w", errA)
	}

	// Читаем колонку B (B2:B12) для описания
	respB, errB := sheetsService.Spreadsheets.Values.Get(spreadsheetID, fmt.Sprintf("%s!%s", teacherSheet, teacherReadRangeB)).Context(ctx).Do()
	if errB != nil {
		return nil, fmt.Errorf("ошибка получения данных из Sheets (B): %w", errB)
	}

	info := make(map[string]string)

	// Функция проверки наличия данных в строке:
	getData := func(rowIndex int) string {
		if len(respA.Values) > rowIndex && len(respA.Values[rowIndex]) > 0 {
			if str, ok := respA.Values[rowIndex][0].(string); ok {
				return str
			}
		}
		return ""
	}

	// A2 (индекс 0): Name
	if name := getData(0); name != "" {
		info["name"] = name
	} else {
		info["name"] = "Не указано"
	}

	// A4 (индекс 2): Photo URL/ID
	info["photo"] = getData(2)

	// A6 (индекс 4): Audio URL/ID
	info["audio"] = getData(4)

	// A8 (индекс 6): Video URL/ID
	info["video"] = getData(6)

	// A10 (индекс 8): Contacts
	if contacts := getData(8); contacts != "" {
		info["contacts"] = contacts
	} else {
		info["contacts"] = "Не указано"
	}

	// Чтение Описания из столбца B и объединение строк
	var descriptionLines []string
	if len(respB.Values) > 0 {
		for _, row := range respB.Values {
			if len(row) > 0 {
				descriptionLines = append(descriptionLines, row[0].(string))
			} else {
				descriptionLines = append(descriptionLines, "")
			}
		}
	}

	info["description"] = strings.Join(descriptionLines, "\n")

	return info, nil
}

func loadTestFromSheets(service *sheets.Service, spreadsheetID string, sheetName string) ([]TestQuestion, error) {
	readRange := fmt.Sprintf("%s!%s", sheetName, readRangeA2toF)
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

		// ФИЛЬТР: Исключаем служебные вкладки
		if strings.Contains(titleLower, "leaderboard") || strings.Contains(titleLower, "results") || title == teacherSheet {
			continue
		}

		testTitles = append(testTitles, title)
	}
	return testTitles, nil
}

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

		go func() {
			if err := updateLeaderboard(); err != nil {
				log.Printf("Ошибка при обновлении Leaderboard после теста: %v", err)
			}
		}()

		buttonLK := tgbotapi.NewInlineKeyboardButtonData("ЛК", "show_lk")
		buttonTests := tgbotapi.NewInlineKeyboardButtonData("Тесты", "start_tests")
		backToMain := tgbotapi.NewInlineKeyboardButtonData("⏪ Назад", "show_start_menu")

		keyboardRow1 := tgbotapi.NewInlineKeyboardRow(buttonTests, buttonLK)
		keyboardRow2 := tgbotapi.NewInlineKeyboardRow(backToMain)
		postTestKeyboard := tgbotapi.NewInlineKeyboardMarkup(keyboardRow1, keyboardRow2)

		finalMsg := tgbotapi.NewMessage(chatID, finalText)
		finalMsg.ReplyMarkup = postTestKeyboard
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

func writeResultToSheets(service *sheets.Service, userID int64, username string, currentScore int, totalQuestions int, testName string) error {
	ctx := context.Background()

	resultSheetName := testName
	readRange := fmt.Sprintf("%s!%s", resultSheetName, readRangeH2toK)
	writeRange := fmt.Sprintf("%s!%s", resultSheetName, writeRangeHtoK)

	resp, err := service.Spreadsheets.Values.Get(spreadsheetID, readRange).Context(ctx).Do()
	if err != nil {
		log.Printf("Предупреждение: Не удалось прочитать результаты из %s. Ошибка: %v", resultSheetName, err)
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
