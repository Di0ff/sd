package main

import (
	"encoding/json"
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

type RSVPRequest struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Email string `json:"email"`
}

type storedRSVP struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Email string `json:"email"`
	At    string `json:"at"`
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

	weddingDateStr := strings.TrimSpace(os.Getenv("WEDDING_DATE"))
	if weddingDateStr != "" {
		weddingDate, err := time.ParseInLocation("2006-01-02", weddingDateStr, time.Local)
		if err != nil {
			log.Printf("WEDDING_DATE неверный формат (нужен 2006-01-02), напоминания отключены: %v", err)
		} else {
			go runReminderLoop(client, fromEmail, store, reminderSent, weddingDate)
		}
	}

	mux := http.NewServeMux()

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

		_ = store.append(storedRSVP{Name: name, Phone: phone, Email: email, At: time.Now().UTC().Format(time.RFC3339)})

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

// formatExportDate переводит RFC3339 (2026-02-13T18:55:36Z) в вид "13.02.2026 18:55"
func formatExportDate(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Format("02.01.2006 15:04")
}

// runReminderLoop раз в сутки проверяет: если сегодня «дата свадьбы − 10 дней», шлёт напоминание гостям с почтой.
func runReminderLoop(client *resend.Client, fromEmail string, store *rsvpStore, sent *reminderSentStore, weddingDate time.Time) {
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
			var toSend []string
			for _, r := range list {
				e := strings.TrimSpace(strings.ToLower(r.Email))
				if e != "" && !already[e] {
					toSend = append(toSend, r.Email)
				}
			}
			body := `<p>Привет!</p><p>Напоминаем: через 10 дней наша свадьба.</p><p>Очень ждём вас!</p>`
			for _, to := range toSend {
				_, err := client.Emails.Send(&resend.SendEmailRequest{
					From:    fromEmail,
					To:      []string{to},
					Subject: "Через 10 дней — ждём вас!",
					Html:    body,
				})
				if err != nil {
					log.Printf("напоминание %s: %v", to, err)
				}
			}
			if len(toSend) > 0 {
				_ = sent.add(toSend)
				log.Printf("напоминания: отправлено %d гостям", len(toSend))
			}
		}
		sleepUntilNextCheck()
	}
}
