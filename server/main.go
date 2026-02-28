package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/resend/resend-go/v2"
	"github.com/xuri/excelize/v2"
)

const (
	maxBodySize     = 4 << 10     // 4 KB
	rateLimitNum    = 5           // запросов
	rateLimitWindow = time.Minute // в минуту с одного IP
)

// Telegram user store
type tgUserStore struct {
	mu   sync.Mutex
	path string
}

type tgUser struct {
	ChatID int64  `json:"chat_id"`
	Phone  string `json:"phone"` // нормализованный (только цифры)
	Name   string `json:"name"`
}

func (s *tgUserStore) get(phone string) (*tgUser, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.load()
	if err != nil {
		return nil, false
	}
	phoneNorm := normalizePhone(phone)
	for _, u := range users {
		if normalizePhone(u.Phone) == phoneNorm {
			return &u, true
		}
	}
	return nil, false
}

func (s *tgUserStore) save(user tgUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.load()
	if err != nil {
		return err
	}
	phoneNorm := normalizePhone(user.Phone)
	// Обновляем или добавляем
	found := false
	for i, u := range users {
		if u.Phone == phoneNorm {
			users[i] = user
			found = true
			break
		}
	}
	if !found {
		users = append(users, user)
	}
	return s.saveUsers(users)
}

func (s *tgUserStore) load() ([]tgUser, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var users []tgUser
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, err
	}
	return users, nil
}

func (s *tgUserStore) saveUsers(users []tgUser) error {
	dir := filepath.Dir(s.path)
	_ = os.MkdirAll(dir, 0755)
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

func (s *tgUserStore) list() ([]tgUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Telegram client
type tgClient struct {
	token  string
	apiURL string
}

func newTelegramClient(token string) *tgClient {
	return &tgClient{
		token:  token,
		apiURL: "https://api.telegram.org/bot" + token,
	}
}

func (t *tgClient) sendMessage(chatID int64, text, parseMode string) error {
	url := t.apiURL + "/sendMessage"
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	data, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API error: %s", string(body))
	}
	return nil
}

func (t *tgClient) sendWebApp(chatID int64, text, url, buttonText string) error {
	apiURL := t.apiURL + "/sendMessage"

	// Keyboard с Web App кнопкой
	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]interface{}{
			{
				{
					"text": buttonText,
					"web_app": map[string]string{
						"url": url,
					},
				},
			},
		},
	}

	payload := map[string]interface{}{
		"chat_id":      chatID,
		"text":         text,
		"parse_mode":   "Markdown",
		"reply_markup": keyboard,
	}

	data, _ := json.Marshal(payload)
	resp, err := http.Post(apiURL, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API error: %s", string(body))
	}
	return nil
}

func (t *tgClient) sendMessageWithCancel(chatID int64, text, cancelText string) error {
	apiURL := t.apiURL + "/sendMessage"

	// Keyboard с кнопкой отмены
	keyboard := map[string]interface{}{
		"inline_keyboard": [][]map[string]interface{}{
			{
				{
					"text": cancelText,
					"callback_data": "cancel_rsvp",
				},
			},
		},
	}

	payload := map[string]interface{}{
		"chat_id":      chatID,
		"text":         text,
		"parse_mode":   "Markdown",
		"reply_markup": keyboard,
	}

	data, _ := json.Marshal(payload)
	resp, err := http.Post(apiURL, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API error: %s", string(body))
	}
	return nil
}

func normalizePhone(phone string) string {
	var result strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			result.WriteRune(r)
		}
	}
	return result.String()
}

type RSVPRequest struct {
	Name           string `json:"name"`
	Phone          string `json:"phone"`
	Email          string `json:"email"`
	TelegramChatID *int64 `json:"telegram_chat_id,omitempty"`
}

type storedRSVP struct {
	Name           string `json:"name"`
	Phone          string `json:"phone"`
	Email          string `json:"email"`
	TelegramChatID *int64 `json:"telegram_chat_id,omitempty"`
	At             string `json:"at"`
}

type rsvpLimiter struct {
	mu     sync.Mutex
	counts map[string][]time.Time
}

func (r *rsvpLimiter) allow(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	cut := now.Add(-rateLimitWindow)
	times := r.counts[ip]
	var kept []time.Time
	for _, t := range times {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rateLimitNum {
		return false
	}
	r.counts[ip] = append(kept, now)
	return true
}

type rsvpStore struct {
	mu   sync.Mutex
	path string
}

func (s *rsvpStore) append(entry storedRSVP) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []storedRSVP
	data, err := os.ReadFile(s.path)
	if err == nil {
		_ = json.Unmarshal(data, &list)
	}
	list = append(list, entry)
	data, err = json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	_ = os.MkdirAll(dir, 0755)
	return os.WriteFile(s.path, data, 0644)
}

func (s *rsvpStore) list() ([]storedRSVP, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var list []storedRSVP
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

type reminderSentStore struct {
	mu   sync.Mutex
	path string
}

func (s *reminderSentStore) list() (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	out := make(map[string]bool)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for _, e := range list {
		out[strings.TrimSpace(strings.ToLower(e))] = true
	}
	return out, nil
}

func (s *reminderSentStore) add(emails []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := os.ReadFile(s.path)
	var list []string
	_ = json.Unmarshal(data, &list)
	seen := make(map[string]bool)
	for _, e := range list {
		seen[strings.TrimSpace(strings.ToLower(e))] = true
	}
	for _, e := range emails {
		e = strings.TrimSpace(strings.ToLower(e))
		if e != "" && !seen[e] {
			seen[e] = true
			list = append(list, e)
		}
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	_ = os.MkdirAll(dir, 0755)
	return os.WriteFile(s.path, data, 0644)
}

func main() {
	resendKey := os.Getenv("RESEND_API_KEY")
	toEmail := strings.TrimSpace(os.Getenv("RSVP_TO_EMAIL"))
	fromEmail := strings.TrimSpace(os.Getenv("RSVP_FROM_EMAIL"))
	staticDir := os.Getenv("STATIC_DIR")
	if staticDir == "" {
		staticDir = ".."
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if resendKey == "" || toEmail == "" {
		log.Fatal("нужны переменные: RESEND_API_KEY, RSVP_TO_EMAIL. Для теста RSVP_FROM_EMAIL можно не задавать (используется onboarding@resend.dev)")
	}
	if fromEmail == "" {
		fromEmail = "Свадьба <onboarding@resend.dev>"
	}

	client := resend.NewClient(resendKey)
	limiter := &rsvpLimiter{counts: make(map[string][]time.Time)}
	exportSecret := strings.TrimSpace(os.Getenv("EXPORT_SECRET"))
	dataPath := os.Getenv("RSVP_DATA_PATH")
	if dataPath == "" {
		dataPath = "data/rsvps.json"
	}
	store := &rsvpStore{path: dataPath}
	reminderSentPath := filepath.Join(filepath.Dir(dataPath), "reminder_sent.json")
	reminderSent := &reminderSentStore{path: reminderSentPath}

	// Telegram
	tgToken := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	tgEnabled := tgToken != ""
	var tg *tgClient
	var tgStore *tgUserStore
	if tgEnabled {
		tg = newTelegramClient(tgToken)
		tgStore = &tgUserStore{path: filepath.Join(filepath.Dir(dataPath), "tg_users.json")}
		log.Printf("Telegram бот инициализирован")
	}

	weddingDateStr := strings.TrimSpace(os.Getenv("WEDDING_DATE"))
	if weddingDateStr != "" {
		weddingDate, err := time.ParseInLocation("2006-01-02", weddingDateStr, time.Local)
		if err != nil {
			log.Printf("WEDDING_DATE неверный формат (нужен 2006-01-02), напоминания отключены: %v", err)
		} else {
			go runReminderLoop(client, fromEmail, store, reminderSent, weddingDate, tg, tgStore)
		}
	}

	// Переменные для подстановки в шаблоны
	placeName := strings.TrimSpace(os.Getenv("WEDDING_PLACE_NAME"))
	if placeName == "" {
		placeName = "Название места, город"
	}
	placeURL := strings.TrimSpace(os.Getenv("WEDDING_PLACE_URL"))
	if placeURL == "" {
		placeURL = "#"
	}
	weddingDateDisplay := strings.TrimSpace(os.Getenv("WEDDING_DATE_DISPLAY"))
	if weddingDateDisplay == "" {
		weddingDateDisplay = "22 июля 2026"
	}
	weddingTimeDisplay := strings.TrimSpace(os.Getenv("WEDDING_TIME_DISPLAY"))
	if weddingTimeDisplay == "" {
		weddingTimeDisplay = "16:30"
	}

	mux := http.NewServeMux()

	// Telegram webhook для регистрации пользователей
	if tgEnabled {
		mux.HandleFunc("/api/tg/webhook", handleTelegramWebhook(tg, tgStore, placeURL, store))
		mux.HandleFunc("/api/tg/init", handleTelegramInit(tg, tgStore))
	}

	mux.HandleFunc("/api/rsvp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			http.Error(w, `{"error":"content-type must be application/json"}`, http.StatusUnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		dec := json.NewDecoder(r.Body)
		var body RSVPRequest
		if err := dec.Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}

		name := strings.TrimSpace(body.Name)
		phone := strings.TrimSpace(body.Phone)
		email := strings.TrimSpace(body.Email)

		if name == "" || len(name) > 200 {
			http.Error(w, `{"error":"name required, max 200 chars"}`, http.StatusBadRequest)
			return
		}
		phoneDigits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, phone)
		if len(phoneDigits) < 10 {
			http.Error(w, `{"error":"phone required, at least 10 digits"}`, http.StatusBadRequest)
			return
		}
		if email != "" && (len(email) > 254 || !strings.Contains(email, "@")) {
			http.Error(w, `{"error":"invalid email"}`, http.StatusBadRequest)
			return
		}

		ip := r.RemoteAddr
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			ip = strings.TrimSpace(strings.Split(xff, ",")[0])
		}
		if !limiter.allow(ip) {
			http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
			return
		}

		subjectName := strings.ReplaceAll(name, "\n", " ")
		subjectName = strings.ReplaceAll(subjectName, "\r", " ")

		// Вам — одна строка: кто ответил и контакты (без формальных подписей)
		noticeHTML := "<p>" + escapeHTML(name) + " — " + escapeHTML(phone)
		if email != "" {
			noticeHTML += ", " + escapeHTML(email)
		}
		noticeHTML += "</p>"
		_, err := client.Emails.Send(&resend.SendEmailRequest{
			From:    fromEmail,
			To:      []string{toEmail},
			Subject: "Ответил(а) " + subjectName,
			Html:    noticeHTML,
		})
		if err != nil {
			log.Printf("resend send: %v", err)
			http.Error(w, `{"error":"failed to send"}`, http.StatusInternalServerError)
			return
		}

		// Гостю — тёплое короткое письмо (если указал почту)
		if email != "" {
			thankHTML := `<p>Привет!</p><p>Мы получили ваш ответ и очень рады, что вы будете с нами.</p><p>Ждём встречи, обнимаем.</p>`
			_, _ = client.Emails.Send(&resend.SendEmailRequest{
				From:    fromEmail,
				To:      []string{email},
				Subject: "Рады, что придёте!",
				Html:    thankHTML,
			})
		}

		// Отправка приглашения в Telegram (если пользователь зарегистрирован)
		if tgEnabled && tg != nil && tgStore != nil && body.TelegramChatID != nil {
			// Сначала сохраняем/обновляем пользователя
			_ = tgStore.save(tgUser{
				ChatID: *body.TelegramChatID,
				Phone:  phone,
				Name:   name,
			})
			log.Printf("TG: сохранён пользователь chat_id=%d, phone=%s", *body.TelegramChatID, phone)
			
			// Теперь ищем и отправляем
			if user, found := tgStore.get(phone); found {
				log.Printf("RSVP: пользователь найден, chat_id=%d, отправка в Telegram", user.ChatID)
				
				// Сообщение с кнопкой отмены
				reply := fmt.Sprintf("✨ *Спасибо, %s!*\n\nМы так рады, что вы будете с нами! 💕\n\n📍 *Детали:*\nДата: %s\nВремя: %s\nМесто: %s\n\nДо встречи на празднике!\n\n_Если ваши планы изменятся, пожалуйста, сообщите нам об этом — просто нажмите на кнопку ниже._",
					escapeMarkdown(name),
					weddingDateDisplay,
					weddingTimeDisplay,
					placeName)
				
				// Отправляем с кнопкой отмены
				go func() {
					if err := tg.sendMessageWithCancel(user.ChatID, reply, "❌ Отменить"); err != nil {
						log.Printf("telegram send to %s: %v", name, err)
					} else {
						log.Printf("telegram отправлено %s (chat_id=%d)", name, user.ChatID)
					}
				}()
			}
		} else {
			// Отправка приглашения в Telegram (если пользователь уже был в базе)
			if tgEnabled && tg != nil && tgStore != nil {
				log.Printf("RSVP: поиск пользователя по телефону: %s", phone)
				if user, found := tgStore.get(phone); found {
					log.Printf("RSVP: пользователь найден, chat_id=%d, отправка в Telegram", user.ChatID)
					
					// Сообщение с кнопкой отмены
					reply := fmt.Sprintf("✨ *Спасибо, %s!*\n\nМы так рады, что вы будете с нами! 💕\n\n📍 *Детали:*\nДата: %s\nВремя: %s\nМесто: %s\n\nДо встречи на празднике!\n\n_Если ваши планы изменятся, пожалуйста, сообщите нам об этом — просто нажмите на кнопку ниже._",
						escapeMarkdown(name),
						weddingDateDisplay,
						weddingTimeDisplay,
						placeName)
					
					go func() {
						if err := tg.sendMessageWithCancel(user.ChatID, reply, "❌ Отменить"); err != nil {
							log.Printf("telegram send to %s: %v", name, err)
						} else {
							log.Printf("telegram отправлено %s (chat_id=%d)", name, user.ChatID)
						}
					}()
				} else {
					log.Printf("RSVP: пользователь НЕ найден в tg_users.json")
				}
			}
		}

		_ = store.append(storedRSVP{
			Name:           name,
			Phone:          phone,
			Email:          email,
			TelegramChatID: body.TelegramChatID,
			At:             time.Now().UTC().Format(time.RFC3339),
		})

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})

	mux.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "", http.StatusMethodNotAllowed)
			return
		}
		key := r.Header.Get("X-Export-Key")
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		if exportSecret == "" || key != exportSecret {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		list, err := store.list()
		if err != nil {
			log.Printf("export list: %v", err)
			http.Error(w, `{"error":"failed to load data"}`, http.StatusInternalServerError)
			return
		}
		f := excelize.NewFile()
		sheet := "Ответы"
		idx, _ := f.NewSheet(sheet)
		f.SetActiveSheet(idx)
		f.DeleteSheet("Sheet1")
		headers := []string{"ФИО", "Телефон", "Почта", "Дата"}
		for i, h := range headers {
			cell, _ := excelize.CoordinatesToCellName(i+1, 1)
			_ = f.SetCellValue(sheet, cell, h)
		}
		for row, entry := range list {
			r := strconv.Itoa(row + 2)
			_ = f.SetCellValue(sheet, "A"+r, entry.Name)
			_ = f.SetCellValue(sheet, "B"+r, entry.Phone)
			_ = f.SetCellValue(sheet, "C"+r, entry.Email)
			_ = f.SetCellValue(sheet, "D"+r, formatExportDate(entry.At))
		}
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", `attachment; filename="rsvp.xlsx"`)
		if err := f.Write(w); err != nil {
			log.Printf("export write: %v", err)
		}
	})

	fs := http.FileServer(http.Dir(staticDir))
	mux.Handle("/", indexWithPlace(staticDir, placeName, placeURL, weddingDateDisplay, weddingTimeDisplay, fs))

	addr := ":" + port
	log.Printf("слушаем %s, статика: %s", addr, staticDir)
	if err := http.ListenAndServe(addr, cors(mux)); err != nil {
		log.Fatal(err)
	}
}

// indexWithPlace отдаёт главную страницу с подстановкой WEDDING_PLACE_* и WEDDING_* из env, остальное — через fs.
func indexWithPlace(staticDir, placeName, placeURL, weddingDateDisplay, weddingTimeDisplay string, fs http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "" {
			fs.ServeHTTP(w, r)
			return
		}
		data, err := os.ReadFile(filepath.Join(staticDir, "index.html"))
		if err != nil {
			fs.ServeHTTP(w, r)
			return
		}
		html := string(data)
		html = strings.ReplaceAll(html, "{{WEDDING_PLACE_NAME}}", escapeHTML(placeName))
		html = strings.ReplaceAll(html, "{{WEDDING_PLACE_URL}}", escapeHTML(placeURL))
		html = strings.ReplaceAll(html, "{{WEDDING_DATE_DISPLAY}}", escapeHTML(weddingDateDisplay))
		html = strings.ReplaceAll(html, "{{WEDDING_TIME_DISPLAY}}", escapeHTML(weddingTimeDisplay))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(html))
	})
}

func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

func escapeMarkdown(s string) string {
	// Экранируем символы Markdown для Telegram
	s = strings.ReplaceAll(s, "_", "\\_")
	s = strings.ReplaceAll(s, "*", "\\*")
	s = strings.ReplaceAll(s, "[", "\\[")
	s = strings.ReplaceAll(s, "`", "\\`")
	return s
}

// formatExportDate переводит RFC3339 (2026-02-13T18:55:36Z) в вид "13.02.2026 18:55"
func formatExportDate(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Format("02.01.2006 15:04")
}

// runReminderLoop раз в сутки проверяет: если сегодня «дата свадьбы − 10 дней», шлёт напоминание гостям с почтой и Telegram.
func runReminderLoop(client *resend.Client, fromEmail string, store *rsvpStore, sent *reminderSentStore, weddingDate time.Time, tg *tgClient, tgStore *tgUserStore) {
	reminderDay := weddingDate.AddDate(0, 0, -10)
	reminderYear, reminderMonth, reminderDayNum := reminderDay.Date()

	sleepUntilNextCheck := func() {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.Local)
		if now.After(next) || now.Equal(next) {
			next = next.AddDate(0, 0, 1)
		}
		d := next.Sub(now)
		if d < time.Minute {
			d = time.Minute
		}
		time.Sleep(d)
	}

	// первый запуск через минуту, чтобы не мешать старту
	time.Sleep(time.Minute)

	for {
		now := time.Now()
		y, m, d := now.Date()
		if y == reminderYear && m == reminderMonth && d == reminderDayNum {
			list, err := store.list()
			if err != nil {
				log.Printf("напоминания: не загрузить список: %v", err)
				sleepUntilNextCheck()
				continue
			}
			already, err := sent.list()
			if err != nil {
				log.Printf("напоминания: не загрузить sent: %v", err)
				sleepUntilNextCheck()
				continue
			}
			var toSendEmail []string
			for _, r := range list {
				e := strings.TrimSpace(strings.ToLower(r.Email))
				if e != "" && !already[e] {
					toSendEmail = append(toSendEmail, r.Email)
				}
			}
			emailBody := `<p>Привет!</p><p>Напоминаем: через 10 дней наша свадьба.</p><p>Очень ждём вас!</p>`
			for _, to := range toSendEmail {
				_, err := client.Emails.Send(&resend.SendEmailRequest{
					From:    fromEmail,
					To:      []string{to},
					Subject: "Через 10 дней — ждём вас!",
					Html:    emailBody,
				})
				if err != nil {
					log.Printf("напоминание email %s: %v", to, err)
				}
			}
			if len(toSendEmail) > 0 {
				_ = sent.add(toSendEmail)
				log.Printf("напоминания email: отправлено %d гостям", len(toSendEmail))
			}

			// Telegram напоминания
			if tg != nil && tgStore != nil {
				tgUsers, err := tgStore.list()
				if err != nil {
					log.Printf("напоминания TG: не загрузить пользователей: %v", err)
				} else {
					tgMessage := "💌 *Напоминание о свадьбе!*\n\nПривет! Напоминаем, что через 10 дней наша свадьба.\n\nОчень ждём вас на празднике!\n\n💕 Александр & Дарья"
					sentCount := 0
					for _, user := range tgUsers {
						if err := tg.sendMessage(user.ChatID, tgMessage, "Markdown"); err != nil {
							log.Printf("напоминание TG %s: %v", user.Name, err)
						} else {
							sentCount++
						}
					}
					if sentCount > 0 {
						log.Printf("напоминания TG: отправлено %d гостям", sentCount)
					}
				}
			}
		}
		sleepUntilNextCheck()
	}
}

// Telegram webhook handler
func handleTelegramWebhook(tg *tgClient, store *tgUserStore, placeURL string, rsvpStore *rsvpStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var update struct {
			Message *struct {
				Chat struct {
					ID   int64  `json:"id"`
					Type string `json:"type"`
				} `json:"chat"`
				From *struct {
					ID        int64  `json:"id"`
					FirstName string `json:"first_name"`
					Username  string `json:"username"`
				} `json:"from"`
				Text string `json:"text"`
			} `json:"message"`
			CallbackQuery *struct {
				ID     string `json:"id"`
				From   *struct {
					ID int64 `json:"id"`
				} `json:"from"`
				Data string `json:"data"`
			} `json:"callback_query"`
		}

		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Обработка callback query (кнопки)
		if update.CallbackQuery != nil {
			chatID := update.CallbackQuery.From.ID
			data := update.CallbackQuery.Data
			
			if data == "cancel_rsvp" {
				// Удаляем RSVP пользователя
				_ = cancelRSVPByChatID(rsvpStore, store, chatID)
				
				// Отвечаем на callback
				answerCallback(tg, update.CallbackQuery.ID)
				
				// Отправляем подтверждение отмены
				_ = tg.sendMessage(chatID, "✅ Отменено.\n\nЕсли передумаете — заполните форму снова, мы будем рады! 💕", "")
			}
			
			w.WriteHeader(http.StatusOK)
			return
		}

		if update.Message == nil {
			w.WriteHeader(http.StatusOK)
			return
		}

		chatID := update.Message.Chat.ID
		userName := ""
		if update.Message.From != nil {
			if update.Message.From.Username != "" {
				userName = "@" + update.Message.From.Username
			} else {
				userName = update.Message.From.FirstName
			}
		}

		text := update.Message.Text

		// Обработка /start
		if text == "/start" {
			// URL для Web App — всегда сайт, а не карта
			webAppURL := "https://alexandr-i-daria.ru"
			
			reply := "🎉 *Привет!*\n\nМы очень рады, что вы с нами! 💕\n\nПожалуйста, заполните небольшую форму — это поможет нам всё организовать наилучшим образом:\n\nНажмите на кнопку ниже:"
			
			// Отправляем текст с кнопкой Web App
			tg.sendWebApp(chatID, reply, webAppURL, "🎊 Я приду!")
			w.WriteHeader(http.StatusOK)
			return
		}

		// Обработка /phone +79990000000
		if strings.HasPrefix(text, "/phone ") {
			phone := strings.TrimSpace(strings.TrimPrefix(text, "/phone "))
			if phone != "" {
				_ = store.save(tgUser{
					ChatID: chatID,
					Phone:  phone,
					Name:   userName,
				})
				reply := fmt.Sprintf("✅ *Отлично!*\n\nВаш номер %s сохранён.\n\nТеперь, когда вы заполните форму RSVP, мы отправим вам приглашение здесь!", phone)
				_ = tg.sendMessage(chatID, reply, "Markdown")
			} else {
				_ = tg.sendMessage(chatID, "❌ Пожалуйста, укажите номер после `/phone`", "")
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		// Обработка номера телефона в любом формате (сохраняем)
		phoneDigits := normalizePhone(text)
		if len(phoneDigits) >= 10 {
			_ = store.save(tgUser{
				ChatID: chatID,
				Phone:  text,
				Name:   userName,
			})
			reply := fmt.Sprintf("✅ *Отлично!*\n\nВаш номер %s сохранён.\n\nТеперь, когда вы заполните форму RSVP, мы отправим вам приглашение здесь!", text)
			_ = tg.sendMessage(chatID, reply, "Markdown")
		}

		w.WriteHeader(http.StatusOK)
	}
}

// handleTelegramInit — сохранение chat_id при открытии сайта из Telegram
func handleTelegramInit(tg *tgClient, store *tgUserStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			ChatID    int64  `json:"chat_id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
			Phone     string `json:"phone"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}

		if req.ChatID == 0 {
			http.Error(w, `{"error":"chat_id required"}`, http.StatusBadRequest)
			return
		}

		name := req.FirstName
		if req.Username != "" {
			name = "@" + req.Username
		}
		if name == "" {
			name = "Telegram User"
		}

		if err := store.save(tgUser{
			ChatID: req.ChatID,
			Phone:  req.Phone,
			Name:   name,
		}); err != nil {
			log.Printf("tg init save: %v", err)
			http.Error(w, `{"error":"failed to save"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}
}

// answerCallback отвечает на callback query
func answerCallback(tg *tgClient, callbackID string) {
	apiURL := tg.apiURL + "/answerCallbackQuery"
	payload := map[string]interface{}{
		"callback_query_id": callbackID,
	}
	data, _ := json.Marshal(payload)
	_, _ = http.Post(apiURL, "application/json", bytes.NewReader(data))
}

// cancelRSVPByChatID удаляет RSVP пользователя по chat_id
func cancelRSVPByChatID(rsvpStore *rsvpStore, tgStore *tgUserStore, chatID int64) error {
	// Находим пользователя
	users, err := tgStore.list()
	if err != nil {
		return err
	}
	
	var userPhone string
	for _, u := range users {
		if u.ChatID == chatID {
			userPhone = u.Phone
			break
		}
	}
	
	if userPhone == "" {
		return nil // Пользователь не найден
	}
	
	// Удаляем RSVP из списка
	rsvpStore.mu.Lock()
	defer rsvpStore.mu.Unlock()
	
	var list []storedRSVP
	data, err := os.ReadFile(rsvpStore.path)
	if err == nil {
		_ = json.Unmarshal(data, &list)
	}
	
	// Фильтруем - удаляем записи с этим телефоном
	var newList []storedRSVP
	for _, r := range list {
		if normalizePhone(r.Phone) != normalizePhone(userPhone) {
			newList = append(newList, r)
		}
	}
	
	// Сохраняем обновлённый список
	data, err = json.MarshalIndent(newList, "", "  ")
	if err != nil {
		return err
	}
	
	return os.WriteFile(rsvpStore.path, data, 0644)
}
