package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

type EmailRequest struct {
	ContentType string
	Sender      string `json:"sender"`
	Recipient   string `json:"recipient"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	Expiration  string `json:"expiration"`
	Template    string `json:"template"`
}

type TextRequest struct {
	Recipient string `json:"recipient"`
	Code      string `json:"code"`
}

type newUserReq struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
	IP       string
}

type DB struct {
	Conn *sql.DB
}

type RateLimiter struct {
	mu      sync.Mutex
	clients map[string][]time.Time
	limit   int
	window  time.Duration
}

type DailyLimit struct {
	mu      sync.Mutex
	clients map[string][]time.Time
	Conn    *sql.DB
}

func (rl *RateLimiter) check(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	valid := rl.clients[key][:0]

	for _, t := range rl.clients[key] {
		if t.After(now.Add(-rl.window)) {
			valid = append(valid, t)
		}
	}

	if len(valid) >= rl.limit {
		rl.clients[key] = valid
		return false
	}

	rl.clients[key] = append(valid, now)
	return true

}

func (dl *DailyLimit) dailyLimitCheck(apikey string) bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	now := time.Now()
	valid := dl.clients[apikey][:0]

	for _, t := range dl.clients[apikey] {
		if t.After(now.Add(-(time.Duration(24) * time.Hour))) {
			valid = append(valid, t)
		}
	}

	var limit int = 15

	dl.Conn.QueryRow(`
		SELECT reqsperday FROM users WHERE api_key = ?
	`, apikey).Scan(&limit)

	if len(valid) >= limit {
		return false
	}

	return true
}

func (dl *DailyLimit) dailyLimitAdd(key string) {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	now := time.Now()
	valid := dl.clients[key][:0]

	for _, t := range dl.clients[key] {
		if t.After(now.Add(-(time.Duration(24) * time.Hour))) {
			valid = append(valid, t)
		}
	}

	dl.clients[key] = append(valid, now)
}

func (dl *DailyLimit) quota(apikey string) (username, email string, reqs_sent, reqs_remaining, daily_quota int, reset_time time.Time, time_remaining float64, times_sent []time.Time) {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	now := time.Now()
	valid := dl.clients[apikey][:0]

	for _, t := range dl.clients[apikey] {
		if t.After(now.Add(-(time.Duration(24) * time.Hour))) {
			valid = append(valid, t)
		}
	}

	m_reset_time := now.Add(time.Duration(24) * time.Hour)
	if len(valid) != 0 {
		m_reset_time = valid[len(valid)-1].Add(time.Duration(24) * time.Hour)
	}
	m_time_remaining := time.Until(m_reset_time).Hours()

	var limit int = 15
	var m_username, m_email string

	thiserr := dl.Conn.QueryRow(`
		SELECT reqsperday, username, email FROM users WHERE api_key = ?
	`, apikey).Scan(&limit, &m_username, &m_email)

	dl.clients[apikey] = valid
	if thiserr != nil {
		return "error", "error", len(valid), (limit - len(valid)), limit, m_reset_time, m_time_remaining, valid
	}
	return m_username, m_email, len(valid), (limit - len(valid)), limit, m_reset_time, m_time_remaining, valid
}

func (rl *RateLimiter) cleanup() {
	for range time.Tick(time.Minute) {
		rl.mu.Lock()
		for key, timestamps := range rl.clients {
			if len(timestamps) == 0 || timestamps[len(timestamps)-1].Before(time.Now().Add(-rl.window)) { // honestly i probably should have used unix timestamps for this too but idrc
				delete(rl.clients, key)
			}
		}
		rl.mu.Unlock()
	}
}

func (dl *DailyLimit) cleanup() {
	for range time.Tick(time.Minute) {
		dl.mu.Lock()
		for key, timestamps := range dl.clients {
			if len(timestamps) == 0 || timestamps[len(timestamps)-1].Before(time.Now().Add(-(time.Duration(24) * time.Hour))) {
				delete(dl.clients, key)
			}
		}
		dl.mu.Unlock()
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {

	switch message[:3] {
	case "555":
		message = "A recipient email address may not have been provided."
		status = 400
	case "T": // token expired
		status = 500
	}

	writeJSON(w, status, map[string]string{"error": message})
}

func sendSMS(req TextRequest) error {
	out, err := exec.Command("python", "send-text.py", req.Recipient, req.Code).Output() // i really need to find a better way than running slow python everytime
	if err != nil {
		return fmt.Errorf("500%s", err.Error())
	}
	if string(out) == "success" {
		return nil
	}
	fmt.Println(string(out))
	return fmt.Errorf(string(out))
}

func sendEmail(req EmailRequest) error {

	pass := os.Getenv("SMTP_PASS")
	email := os.Getenv("SMTP_EMAIL")

	if pass == "" {
		return fmt.Errorf("SMTP_PASS not set")
	}
	if email == "" {
		return fmt.Errorf("SMTP_EMAIL not set")
	}

	fromHeader := `"` + req.Sender + `"` + " <no-reply@3272010.xyz>"
	host := "smtp.gmail.com"
	addr := host + ":587"

	auth := smtp.PlainAuth("", email, pass, host)

	if req.ContentType == "html" {
		content, err := os.ReadFile("./templates/" + req.Template + ".html")
		if err != nil {
			return err
		}

		if req.Expiration == "" {
			req.Expiration = "sometime"
		} else if len(req.Expiration) > 10 {
			req.Expiration = req.Expiration[:10]
		}

		text := string(content)
		text = strings.ReplaceAll(text, "{{.VerifyURL}}", req.Body)
		text = strings.ReplaceAll(text, "{{.OTPCode}}", req.Body)
		text = strings.ReplaceAll(text, "{{.Expiration}}", req.Expiration)
		req.Body = text
	}

	msg := []byte("From: " + fromHeader + "\r\n" +
		"To: " + req.Recipient + "\r\n" +
		"Subject: " + req.Subject + "\r\n" +
		"Content-Type: text/" + req.ContentType + "; charset=\"utf-8\"\r\n" +
		"\r\n" +
		req.Body + "\r\n")

	if err := smtp.SendMail(addr, auth, email, []string{req.Recipient}, msg); err != nil {
		return err
	}
	return nil
}

func getIP(r *http.Request) string {
	ip := r.Header.Get("X-Forwarded-For")
	if ip != "" {
		return strings.Split(ip, ",")[0]
	}

	ip = r.Header.Get("X-Real-IP")
	if ip != "" {
		return ip
	}

	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (db *DB) createUser(req newUserReq) error {

	if req.Token != "" {
		var exists bool

		err := db.Conn.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM tokens WHERE token = ?
			)
		`, req.Token).Scan(&exists)

		if err != nil || !exists {
			return fmt.Errorf("302invalid%20token") // spaces should automatically be encoded to %20 hopefully. edit: it does but just to be safe im harcoding it in.
		}

		err = db.Conn.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM tokens WHERE ip = ?
			)
		`, req.IP).Scan(&exists)

		if err != nil || !exists { // thinking about removing this
			return fmt.Errorf("302ip%20mismatch")
		}

		if req.Password != "" {

			// time to add annoying password safety checkers

			matched, _ := regexp.MatchString(`[^a-zA-Z0-9]`, req.Password)
			lowercase, _ := regexp.MatchString("[a-z]", req.Password)
			uppercase, _ := regexp.MatchString(`[A-Z]`, req.Password)

			if len(req.Password) < 8 {
				return fmt.Errorf("Password must be at least 8 characters long.")
			} else if !strings.ContainsAny(req.Password, "1234567890") {
				return fmt.Errorf("Password must have at least one number.")
			} else if !matched {
				return fmt.Errorf("Password must have at least one non-alphanumeric character.")
			} else if !lowercase {
				return fmt.Errorf("Password must have at least one lowercase character.")
			} else if !uppercase {
				return fmt.Errorf("Password must have at least one uppercase character.")
			}

			hash := sha256.Sum256([]byte(req.Password))
			req.Password = hex.EncodeToString(hash[:])

			err = db.Conn.QueryRow(`
				SELECT email, username FROM tokens WHERE token = ?
			`, req.Token).Scan(&req.Email, &req.Username)

			if err != nil {
				return fmt.Errorf("500%s", err.Error())
			}

			b := make([]byte, 16)
			rand.Read(b)
			apiKey := hex.EncodeToString(b)

			_, err = db.Conn.Exec(`
				INSERT INTO users (api_key, email, username, password, reqsperday)
				VALUES (?, ?, ?, ?, ?)
			`, apiKey, req.Email, req.Username, req.Password, 15)

			if err != nil {
				return fmt.Errorf("500%s", err.Error())
			}

			_, err = db.Conn.Exec(`
				DELETE FROM tokens WHERE token = ?
			`, req.Token)

			if err != nil {
				return fmt.Errorf("500%s", err.Error())
			}

			return fmt.Errorf("success")
		}

		return fmt.Errorf("No password was provided.")

	}

	if _, err := mail.ParseAddress(req.Email); err != nil {
		return fmt.Errorf("An invalid email address was given.")
	}

	nonalphanumeric, _ := regexp.MatchString("[^a-zA-Z0-9]", req.Username)

	if nonalphanumeric {
		return fmt.Errorf("Only alphanumeric characters in the username are allowed!")
	}

	var usernameExists bool
	var emailExists bool

	err := db.Conn.QueryRow(`
        SELECT EXISTS(
            SELECT 1 FROM users WHERE email = ?
        )
    `, req.Email).Scan(&emailExists)

	if err != nil || emailExists {
		return fmt.Errorf("An account with this email already exists!")
	}

	err = db.Conn.QueryRow(`
        SELECT EXISTS(
            SELECT 1 FROM users WHERE username = ?
        )
    `, req.Username).Scan(&usernameExists)

	if err != nil || usernameExists {
		return fmt.Errorf("Username already exists!")
	}

	b := make([]byte, 16)
	rand.Read(b)

	_, err = db.Conn.Exec(`
		INSERT INTO tokens (token, ip, username, email, expires_at)
		VALUES (?, ?, ?, ?, ?)
	`, hex.EncodeToString(b), req.IP, req.Username, req.Email, (time.Now().Unix() + 600))

	if err != nil {
		if err.Error() == "UNIQUE constraint failed: tokens.email" || err.Error() == "UNIQUE constraint failed: tokens.username" {
			return fmt.Errorf("An email request was already sent!")
		}
		return err
	}

	var eReq EmailRequest
	eReq.Sender = "3272010 Authentication"
	eReq.Recipient = req.Email
	eReq.Subject = "Email Verification"
	eReq.ContentType = "html"
	eReq.Template = "link"
	eReq.Expiration = "5 minutes"
	eReq.Body = "https://authenticator.3272010.xyz/set-password?token=" + hex.EncodeToString(b) // reminder to change link

	err = sendEmail(eReq)

	if err != nil {
		return err
	}

	return nil
}

func (db *DB) cleanupTokens() {
	for range time.Tick(time.Minute) {
		_, err := db.Conn.Exec(`
			DELETE FROM tokens WHERE expires_at < ?
		`, time.Now().Unix())

		if err != nil {
			panic(err)
		}
	}
}

func (db *DB) initDB() error {
	_, err := db.Conn.Exec(`
        CREATE TABLE IF NOT EXISTS users (
			api_key TEXT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			username TEXT NOT NULL UNIQUE,
			password TEXT NOT NULL UNIQUE,
			reqsperday INT NOT NULL
		);
    `)
	if err != nil {
		return err
	}

	_, err = db.Conn.Exec(`
        CREATE TABLE tokens (
			token TEXT PRIMARY KEY,
			ip TEXT,
			device TEXT,
			username TEXT NOT NULL,
			email TEXT NOT NULL,
			expires_at INTEGER NOT NULL
		);
    `)
	if err != nil {
		return err
	}

	return nil
}

func (db *DB) authenticateClient(apikey string) bool {
	if len(apikey) == 32 {
		var exists bool

		err := db.Conn.QueryRow(`
				SELECT EXISTS(
					SELECT 1 FROM users WHERE api_key = ?
				)
			`, apikey).Scan(&exists)
		if err != nil {
			return false
		}

		return exists

	}

	return false
}

func (db *DB) resetApiKey(username, password string) (string, error) {
	hash := sha256.Sum256([]byte(password))
	password = hex.EncodeToString(hash[:])
	var dbpassword string

	err := db.Conn.QueryRow(`
		SELECT password FROM users WHERE username = ?
	`, username).Scan(&dbpassword)

	if err == sql.ErrNoRows {
		return "", fmt.Errorf("User not found")
	} else if dbpassword != password {
		return "", fmt.Errorf("Incorrect password") // reminder to rate limit this so no brute forcing
	}

	b := make([]byte, 16)
	rand.Read(b)
	newApiKey := hex.EncodeToString(b)

	_, err = db.Conn.Exec(`
		UPDATE users SET api_key = ? WHERE username = ?
	`, newApiKey, username)

	if err != nil {
		return "", err
	}

	return newApiKey, nil
}

func initRl(db *DB) (signupRl *RateLimiter, reqRl *RateLimiter, dailyLimit *DailyLimit) {
	SRl := RateLimiter{
		clients: make(map[string][]time.Time),
		limit:   10,
		window:  (time.Duration(30) * time.Second),
	}
	go SRl.cleanup()

	RRl := RateLimiter{
		clients: make(map[string][]time.Time),
		limit:   5,
		window:  (time.Duration(60) * time.Second),
	}
	go RRl.cleanup()

	dl := DailyLimit{
		clients: make(map[string][]time.Time),
		Conn:    db.Conn,
	}
	go dl.cleanup()

	return &SRl, &RRl, &dl
}

func newSqlDb() (*DB, error) {
	conn, err := sql.Open("sqlite3", ".db")
	if err != nil {
		return nil, err
	}

	return &DB{Conn: conn}, nil
}

func main() {
	_ = godotenv.Load()

	db, err := newSqlDb()
	if err != nil {
		panic(err)
	}
	db.initDB()
	go db.cleanupTokens()

	signupRl, reqRl, dailyLimit := initRl(db)

	mux := http.NewServeMux()
	site := http.NewServeMux()

	mux.HandleFunc("/sendEmail/", func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if reqRl.check(getIP(r)) {
			if db.authenticateClient(r.Header.Get("Authorization")) {
				if reqRl.check(r.Header.Get("Authorization")) {
					if dailyLimit.dailyLimitCheck(r.Header.Get("Authorization")) {

						var req EmailRequest

						err := json.NewDecoder(r.Body).Decode(&req)
						if err != nil {
							writeError(w, 500, "There was an error in parsing the JSON")
							return
						}

						if _, err := mail.ParseAddress(req.Recipient); err != nil {
							writeError(w, 400, "An invalid recipient email address was given.")
							return
						}
						if strings.ReplaceAll(req.Body, " ", "") == "" {
							writeError(w, 400, "Invalid body provided.")
							return
						}
						if req.Template != "" {
							_, err := os.Stat("./templates/" + req.Template + ".html")
							if os.IsNotExist(err) {
								writeError(w, 400, "Invalid template.")
								return
							}
							req.ContentType = "html"
						} else {
							req.ContentType = "plain"
						}
						if strings.ReplaceAll(req.Sender, " ", "") == "" {
							req.Sender = "Authentication by 3272010.xyz"
						}
						if strings.ReplaceAll(req.Subject, " ", "") == "" {
							req.Subject = "Authentication by 3272010.xyz"
						}

						result := sendEmail(req)

						if result == nil {
							writeJSON(w, 200, "success")
							dailyLimit.dailyLimitAdd(r.Header.Get("Authorization"))
						} else {
							writeError(w, 500, result.Error())
						}
					} else {
						writeError(w, 429, "You have hit the daily limit of free requests.")
					}
				} else {
					writeError(w, 429, "Too many requests. You are being rate limited.")
				}
			} else {
				writeError(w, 401, "Invalid API key provided. Unauthorized. https://authenticator.3272010.xyz/free-api-key")
			}
		} else {
			writeError(w, 429, "Too many requests. This IP is being rate limited.")
		}

	})

	mux.HandleFunc("/sendText/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET") // testing purposes only, reminder to change later
		// w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		var req TextRequest

		req.Code = r.PathValue("code")
		req.Recipient = r.PathValue("number")

		result := sendSMS(req)

		if result != nil {
			if result.Error()[:3] == "500" {
				writeError(w, 500, result.Error()[3:])
				return
			}
			writeError(w, 400, result.Error())
			return
		} else {
			writeJSON(w, 200, "success")
		}

	})

	mux.HandleFunc("/quota/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if reqRl.check(getIP(r)) {
			if db.authenticateClient(r.Header.Get("Authorization")) {
				username, email, reqs_sent, reqs_remaining, daily_quota, reset_time, time_remaining, times_sent := dailyLimit.quota(r.Header.Get("Authorization"))

				writeJSON(w, 200, map[string]any{
					"username":                 username,
					"email":                    email,
					"requests_sent_today":      reqs_sent,
					"requests_remaining_today": reqs_remaining,
					"reset_time":               reset_time,
					"resets_in_hours":          time_remaining,
					"daily_quota":              daily_quota,
					"times_requests_sent":      times_sent,
				})
			} else {
				writeError(w, 401, "Invalid API key provided. Unauthorized. https://authenticator.3272010.xyz/free-api-key")
			}
		} else {
			writeError(w, 429, "Too many requests. This IP is being rate limited.")
		}
	})

	site.HandleFunc("/createUser/", func(w http.ResponseWriter, r *http.Request) {
		// w.Header().Set("Access-Control-Allow-Origin", "https://authenticator.3272010.xyz")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if signupRl.check(getIP(r)) {

			var req newUserReq

			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, 400, "Invalid JSON")
				return
			}

			req.Email, _ = url.QueryUnescape(req.Email)
			req.Username, _ = url.QueryUnescape(req.Username)
			req.Password, _ = url.QueryUnescape(req.Password)
			req.IP = getIP(r)
			req.Token, _ = url.QueryUnescape(req.Token)

			result := db.createUser(req)

			if result == nil {
				writeJSON(w, 200, "success")
			} else {
				if result.Error()[:3] == "500" {
					writeError(w, 500, result.Error()[3:])
				} else if result.Error()[:3] == "302" {
					http.Redirect(w, r, ("https://authenticator.3272010.xyz/sign-up?redir-reason=" + result.Error()[3:]), 302)
				} else if result.Error() == "success" {
					writeJSON(w, 200, "success")
				} else {
					writeError(w, 400, result.Error())
				}
			}
		} else {
			writeError(w, 429, "Too many requests. You are being rate limited.")
		}

	})

	site.HandleFunc("/authenticate/", func(w http.ResponseWriter, r *http.Request) { // reminder to add rsa encryption
		// w.Header().Set("Access-Control-Allow-Origin", "https://authenticator.3272010.xyz")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost {
			http.Redirect(w, r, "https://authenticator.3272010.xyz", 302)
			return
		}

		if reqRl.check(getIP(r)) {
			var data map[string]any
			json.NewDecoder(r.Body).Decode(&data)

			email, _ := data["email"].(string)
			username, _ := data["username"].(string)
			password, _ := data["password"].(string)
			remember, _ := data["remember"].(bool)
			hash := sha256.Sum256([]byte(password))
			password = hex.EncodeToString(hash[:])

			var dbpassword string
			var err error

			if username != "" {
				err = db.Conn.QueryRow(`
					SELECT password FROM users WHERE username = ?
				`, username).Scan(&dbpassword)

				if err != nil {
					writeError(w, 400, "Account with this username does not exist! Please sign up or try again.")
					return
				}
			} else if email != "" {
				err = db.Conn.QueryRow(`
					SELECT password FROM users WHERE email = ?
				`, email).Scan(&dbpassword)

				if err != nil {
					writeError(w, 400, "Account with this email does not exist! Please sign up or try again")
					return
				}
			} else {
				writeError(w, 400, "Please provide an email/username.")
				return
			}

			if dbpassword == password {
				b := make([]byte, 16)
				rand.Read(b)
				session_token := hex.EncodeToString(b)

				expires := time.Now().Add(24 * time.Hour)
				if remember == true {
					expires = time.Now().Add(720 * time.Hour)
				}

				_, err = db.Conn.Exec(`
					INSERT INTO tokens (token, ip, device, username, email, expires_at)
					VALUES (?, ?, ?, ?, ?, ?)
				`, session_token, getIP(r), r.Header.Get("User-Agent"), username, email, expires)

				if err != nil {
					writeError(w, 500, "SQL error: "+err.Error())
				}

				http.SetCookie(w, &http.Cookie{
					Name:     "session_token",
					Value:    session_token,
					Expires:  expires,
					HttpOnly: true,
					Secure:   true,
					Path:     "/",
				})

				writeJSON(w, 200, "success")
			} else {
				writeError(w, 400, "Wrong password.")
				return
			}
		} else {
			writeError(w, 429, "Too many requests. You are being rate limited.")
		}

	})

	site.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := "./static" + r.URL.Path

		if _, err := os.Stat(path); err == nil {
			http.ServeFile(w, r, path)
			return
		}

		http.ServeFile(w, r, "./static/404")
	})

	go http.ListenAndServe(":8080", mux)
	fmt.Println("API running on http://localhost:8080")
	http.ListenAndServe(":3000", site)
	fmt.Println("Site running on http://localhost:3000")
}
