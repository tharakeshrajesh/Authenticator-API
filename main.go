package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

type EmailRequest struct {
	APIKey    string
	Username  string
	Sender    string `json:"sender"`
	Recipient string `json:"recipient"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
}

type newUserReq struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	IP       string "some IP"
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
			if len(timestamps) == 0 || timestamps[len(timestamps)-1].Before(time.Now().Add(-rl.window)) {
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

func sendEmail(req EmailRequest, rateLimiter *RateLimiter) error {

	if rateLimiter.check(req.Username) == false {
		return fmt.Errorf("429")
	}

	// if db.authenticateClient() == false {
	// 	return fmt.Errorf("401")
	// }

	_ = godotenv.Load()

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

	msg := []byte("From: " + fromHeader + "\r\n" +
		"To: " + req.Recipient + "\r\n" +
		"Subject: " + req.Subject + "\r\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
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

func (db *DB) createUser(req newUserReq, rateLimiter *RateLimiter) error {

	if rateLimiter.check(req.IP) == false {
		return fmt.Errorf("429")
	}

	if _, err := mail.ParseAddress(req.Email); err != nil {
		return fmt.Errorf("An invalid email address was given.")
	}

	var usernameExists bool
	var emailExists bool

	err := db.Conn.QueryRow(`
        SELECT EXISTS(
            SELECT 1 FROM clients WHERE email = ?
        )
    `, req.Email).Scan(&emailExists)

	if err != nil {
		return fmt.Errorf("An account with this email already exists!")
	}

	err = db.Conn.QueryRow(`
        SELECT EXISTS(
            SELECT 1 FROM clients WHERE username = ?
        )
    `, req.Username).Scan(&usernameExists)

	if err != nil {
		return fmt.Errorf("Username already exists!")
	}

	b := make([]byte, 16)
	rand.Read(b)

	_, err = db.Conn.Exec(`
		INSERT INTO users (id, email, api_key)
		VALUES (?, ?, ?)
	`, req.Username, req.Email, hex.EncodeToString(b)) // make api generate function better or something idk and hash it

	return fmt.Errorf("500" + err.Error())
}

func (db *DB) initDB() error {
	_, err := db.Conn.Exec(`
        CREATE TABLE IF NOT EXISTS users (
			username INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT NOT NULL UNIQUE,
			api_key TEXT NOT NULL UNIQUE
		);
    `)
	if err != nil {
		return err
	}

	_, err = db.Conn.Exec(`
        CREATE TABLE tokens (
			token TEXT PRIMARY KEY,
			ip TEXT NOT NULL,
			expires_at INTEGER NOT NULL
		);
    `)
	if err != nil {
		return err
	}

	return nil
}

func (db *DB) authenticateClient(apikey, username string) {
	// do later
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
	conn, err := sql.Open("sqlite3", "api-keys.db")
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

	signupRl, reqRl := initRl()

	mux := http.NewServeMux()

	mux.HandleFunc("/sendEmail/", func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

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

		result := sendEmail(req, &reqRl) // omg bro im going to die. what am i doing wrong with this pointer

		if result == nil {
			writeJSON(w, 200, "success")
		} else {
			if result.Error() == "429" {
				writeError(w, 429, "Too many requests. You are being rate limited.")
			} else if result.Error() == "401" {
				writeError(w, 401, "API key not provided. Unauthorized. https://3272010.xyz/free-api-key")
			} else {
				writeError(w, 500, result.Error())
			}
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

		var req newUserReq

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "Invalid JSON")
			return
		}

		req.IP = getIP(r)

		result := db.createUser(req, &signupRl) // and this one

		if result == nil {
			writeJSON(w, 200, "success")
		} else {
			if result.Error() == "429" {
				writeError(w, 429, "Too many requests. You are being rate limited.")
			} else if result.Error()[:3] == "500" {
				writeError(w, 500, result.Error()[3:])
			} else {
				writeError(w, 400, result.Error())
			}
		}

	})

	fmt.Println("Server running on http://localhost:8080")
	http.ListenAndServe(":8080", mux)
}

// im not even going to compile. i havent compiled a single time today and im done bruh. this is the poorest developed api ever probably.
