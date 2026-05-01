package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"os"
	"github.com/joho/godotenv"
)

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func sendEmail(name string, to string, subject string, body string) error {
	_ = godotenv.Load()

	pass := os.Getenv("SMTP_PASS")
	email := os.Getenv("SMTP_EMAIL")

	if pass == "" {
		return fmt.Errorf("SMTP_PASS not set")
	}
	if email == "" {
		return fmt.Errorf("SMTP_EMAIL not set")
	}
	
	fromHeader := `"` + name + `"` + " <no-reply@3272010.xyz>"
	host := "smtp.gmail.com"
	addr := host + ":587"

	auth := smtp.PlainAuth("", email, pass, host)

	msg := []byte("From: " + fromHeader + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
		"\r\n" +
		body + "\r\n")

	if err := smtp.SendMail(addr, auth, email, []string{to}, msg); err != nil {
		return err
	}
	return nil
}

func main() {
	_ = godotenv.Load()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /sendEmail/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		to := r.URL.Query().Get("recipient")
		subject := r.URL.Query().Get("subject")
		body := r.URL.Query().Get("body")

		result := sendEmail(name, to, subject, body)

		if result == nil {
			writeJSON(w, 200, "success")
		} else {
			writeError(w, 500, "idk")
		}
		
	})

	fmt.Println("Server running on http://localhost:8080")
	http.ListenAndServe(":8080", mux)
}