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
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

type EmailRequest struct {
	Sender    string `json:"sender"`
	Recipient string `json:"recipient"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
}

type newUserReq struct {
	Email string `json:"email"`
	Token string `json:"token"`
	IP    string "some IP"
}

type DB struct {
	Conn *sql.DB
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

func sendEmail(req EmailRequest) error {
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

func (db *DB) createUser(req newUserReq) error {

	if _, err := mail.ParseAddress(req.Email); err != nil {
		return fmt.Errorf("An invalid email address was given.")
	}

	var exists bool

	err := db.Conn.QueryRow(`
        SELECT EXISTS(
            SELECT 1 FROM clients WHERE email = ?
        )
    `, req.Email).Scan(&exists)

	if err != nil {
		// checks if token exists. noting so i dont forget

		var expires int64

		err = db.Conn.QueryRow(`
			SELECT expires_at
			FROM tokens
			WHERE token = ? AND ip = ?
		`, req.Token, req.IP).Scan(&expires)

		if err == sql.ErrNoRows {
			return fmt.Errorf("Invalid token or IP mismatch.")
		}
		if err != nil {
			return err
		}
		if time.Now().Unix() > expires {
			return fmt.Errorf("Token expired.")
		}

		// do email verification later

		var maxID int

		err = db.Conn.QueryRow(`
			SELECT MAX(id) FROM users
		`).Scan(&maxID)

		b := make([]byte, 16)
		rand.Read(b)

		_, err = db.Conn.Exec(`
			INSERT INTO users (id, email, api_key)
			VALUES (?, ?, ?)
		`, maxID+1, req.Email, hex.EncodeToString(b)) // make api generate function better or something idk and hash it

		_, err = db.Conn.Exec(`
			DELETE FROM tokens
			WHERE token = ? AND ip = ?
		`, req.Token, req.IP)
	}

	return fmt.Errorf("Email already exists.")
}

func (db *DB) initDB() error {
	_, err := db.Conn.Exec(`
        CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
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

		result := sendEmail(req)

		if result == nil {
			writeJSON(w, 200, "success")
		} else {
			writeError(w, 500, result.Error())
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

		result := db.createUser(req)

		if result == nil {
			writeJSON(w, 200, "success")
		} else {
			writeError(w, 400, result.Error())
		}

	})

	fmt.Println("Server running on http://localhost:8080")
	http.ListenAndServe(":8080", mux)
}
