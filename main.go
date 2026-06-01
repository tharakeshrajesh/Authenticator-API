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

func (rl *RateLimiter) dailyLimitCheck(key string) bool {
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
		return false
	}

	return true
}

func (rl *RateLimiter) dailyLimitAdd(key string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	valid := rl.clients[key][:0]

	for _, t := range rl.clients[key] {
		if t.After(now.Add(-rl.window)) {
			valid = append(valid, t)
		}
	}

	rl.clients[key] = append(valid, now)
}

func (rl *RateLimiter) quota(key string) bool {
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
				INSERT INTO users (api_key, email, username, password)
				VALUES (?, ?, ?, ?)
			`, apiKey, req.Email, req.Username, req.Password)

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
			password TEXT NOT NULL UNIQUE
		);
    `)
	if err != nil {
		return err
	}

	_, err = db.Conn.Exec(`
        CREATE TABLE tokens (
			token TEXT PRIMARY KEY,
			ip TEXT NOT NULL,
			username TEXT NOT NULL UNIQUE,
			email TEXT NOT NULL UNIQUE,
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

func initRl() (signupRl *RateLimiter, reqRl *RateLimiter, dailyLimit *RateLimiter) {
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

	DRl := RateLimiter{
		clients: make(map[string][]time.Time),
		limit:   50,
		window:  (time.Duration(24) * time.Hour),
	}
	go DRl.cleanup()

	return &SRl, &RRl, &DRl
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

	signupRl, reqRl, dailyLimit := initRl()

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
				writeError(w, 401, "API key not provided. Unauthorized. https://authenticator.3272010.xyz/free-api-key")
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
			// i will do later im too lazy rn
		}
	})

	mux.HandleFunc("/ip/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		writeJSON(w, 200, getIP(r))
	})

	mux.HandleFunc("/createUser/", func(w http.ResponseWriter, r *http.Request) { // thinking about changing it from api subdomain to regular one (mux to site)
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
