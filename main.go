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
	"os"
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
	IP       string
	Password string
	Token    string
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
	case "553":
		message = "An invalid recipient email address was given."
		status = 400
	case "555":
		message = "A recipient email address may not have been provided."
		status = 400
	case "T": // token expired
		status = 500
	}

	writeJSON(w, status, map[string]string{"error": message})
}

func sendSMS(req TextRequest) error {
	//do later
	return nil
}

func sendEmail(req EmailRequest) error {
	if _, err := mail.ParseAddress(req.Recipient); err != nil {
		return fmt.Errorf("553")
	}

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

	content, err := os.ReadFile("./templates/" + req.Template + ".html")
	if err != nil {
		return err
	}

	text := string(content)
	text = strings.ReplaceAll(text, "{{.VerifyURL}}", req.Body)
	text = strings.ReplaceAll(text, "{{.OTPCode}}", req.Body)
	text = strings.ReplaceAll(text, "{{.Expiration}}", req.Expiration)

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

		if err != nil {
			return fmt.Errorf("302invalid token") // spaces should automatically be encoded to %20 hopefully
		}

		err = db.Conn.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM tokens WHERE ip = ?
			)
		`, req.IP).Scan(&exists)

		if err != nil {
			return fmt.Errorf("302ip mismatch")
		}

		if req.Password != "" {

			// time to add annoying password safety checkers

			matched, _ := regexp.MatchString(`[^a-zA-Z0-9]`, req.Password)

			if len(req.Password) < 8 {
				return fmt.Errorf("Password must be at least 8 characters long.")
			} else if !strings.ContainsAny(req.Password, "1234567890") {
				return fmt.Errorf("Password must have at least one number.")
			} else if !matched {
				return fmt.Errorf("Password must have at least one non-alphanumeric character.")
			}

			_, err = db.Conn.Exec(`
				INSERT INTO users (api_key, email, username, password)
				VALUES (?, ?, ?, ?)
			`, "this-is-a-placeholder-key-so-please-reset-it", req.Email, req.Username, req.Password) // dont worry, the password is hashed on client side already. if i rememebr to do it...

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

	if err != nil {
		return fmt.Errorf("An account with this email already exists!")
	}

	err = db.Conn.QueryRow(`
        SELECT EXISTS(
            SELECT 1 FROM users WHERE username = ?
        )
    `, req.Username).Scan(&usernameExists)

	if err != nil {
		return fmt.Errorf("Username already exists!")
	}

	var eReq EmailRequest
	eReq.Sender = "3272010 Authentication"
	eReq.Recipient = req.Email
	eReq.Subject = "Email Verification"
	eReq.ContentType = "html"

	b := make([]byte, 16)
	rand.Read(b)

	eReq.Body = "https://authenticator.3272010.xyz/set-password?token=" + hex.EncodeToString(b) // reminder to change link

	sendEmail(eReq)

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
	if len(apikey) == 16 {

		hash := sha256.Sum256([]byte(apikey))
		hex.EncodeToString(hash[:])

		var exists bool

		err := db.Conn.QueryRow(`
				SELECT EXISTS(
					SELECT 1 FROM users WHERE api_key = ?
				)
			`, hash).Scan(&exists)
		if err != nil {
			return false
		}

		return exists

	}

	return false
}

func (db *DB) resetApiKey(username, password string) string {
	//do later
	return ""
}

func initRl() (signupRl *RateLimiter, reqRl *RateLimiter) {
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

	return &SRl, &RRl
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

	signupRl, reqRl := initRl()

	mux := http.NewServeMux()

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

				var req EmailRequest

				err := json.NewDecoder(r.Body).Decode(&req)
				if err != nil {
					writeError(w, 500, "There was an error in parsing the JSON")
					return
				}

				if strings.ReplaceAll(req.Sender, " ", "") == "" {
					req.Sender = "Authentication by 3272010.xyz"
				}
				if strings.ReplaceAll(req.Subject, " ", "") == "" {
					req.Subject = "Authentication by 3272010.xyz"
				}
				if strings.ReplaceAll(req.Body, " ", "") == "" {
					writeError(w, 400, "Invalid body provided.")
					return
				}
				if req.Template != "" {
					_, err := os.Stat("./templates/" + req.Template + ".html")
					if os.IsNotExist(err) {
						writeError(w, 400, "Provided template does not exist.")
						return
					}
					req.ContentType = "html"
				} else {
					req.ContentType = "plain"
				}

				result := sendEmail(req)

				if result == nil {
					writeJSON(w, 200, "success")
				} else {
					writeError(w, 500, result.Error())
				}
			} else {
				writeError(w, 401, "API key not provided. Unauthorized. https://3272010.xyz/free-api-key") // reminder to make this page
			}
		} else {
			writeError(w, 429, "Too many requests. You are being rate limited.")
		}

	})

	mux.HandleFunc("/createUser/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://3272010.xyz") // reminder to change
		w.Header().Set("Access-Control-Allow-Methods", "GET")

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

			req.Token = r.PathValue("token")

			result := db.createUser(req)

			if result == nil {
				writeJSON(w, 200, "success")
			} else {
				if result.Error()[:3] == "500" {
					writeError(w, 500, result.Error()[3:])
				} else if result.Error()[:3] == "302" {
					http.Redirect(w, r, ("/sign-up?redir-reason=" + result.Error()[3:]), 302)
				} else if result.Error() == "success" {
					http.Redirect(w, r, "/log-in?success=1", 302)
				} else {
					writeError(w, 400, result.Error())
				}
			}
		} else {
			writeError(w, 429, "Too many requests. You are being rate limited.")
		}

	})

	fmt.Println("Server running on http://localhost:8080")
	http.Handle("/", http.FileServer(http.Dir("./static"))) //idk if this will override the api functions but hopefully not
	http.ListenAndServe(":8080", mux)
}

// time to compile
