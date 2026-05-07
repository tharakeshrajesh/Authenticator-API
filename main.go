package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"net/http"
	"net/smtp"
	"os"
	"github.com/joho/godotenv"
)

type EmailRequest struct {
    Sender    string `json:"sender"`
    Recipient string `json:"recipient"`
    Subject   string `json:"subject"`
    Body      string `json:"body"`
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	errorcode := message[:3]

	switch message {
	case "553":
		message = "An invalid email address was given."
	//case ""
	}

	writeJSON(w, status, map[string]string{"error": message, "errorcode": errorcode})
}

func sendEmail(req EmailRequest) error {
	_ = godotenv.Load()

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

func main() {
	_ = godotenv.Load()

	mux := http.NewServeMux()

	mux.HandleFunc("/sendEmail/", func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
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

	fmt.Println("Server running on http://localhost:8080")
	http.ListenAndServe(":8080", mux)
}