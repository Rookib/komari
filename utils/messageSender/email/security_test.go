package email

import (
	"net/smtp"
	"testing"
)

func TestSMTPAuthenticationRequiresEncryptedConnection(t *testing.T) {
	for _, auth := range []smtp.Auth{&loginAuth{username: "user", password: "secret"}, &plainAuthWithoutCheck{username: "user", password: "secret"}} {
		if _, response, err := auth.Start(&smtp.ServerInfo{Name: "mail.example", TLS: false}); err == nil || response != nil {
			t.Fatal("SMTP credentials must not be released without TLS")
		}
		if _, _, err := auth.Start(&smtp.ServerInfo{Name: "mail.example", TLS: true}); err != nil {
			t.Fatalf("TLS authentication unexpectedly rejected: %v", err)
		}
	}
}
