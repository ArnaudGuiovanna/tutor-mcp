package auth

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSMTPEmailSenderRequiresSTARTTLS(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("220 test SMTP\r\n"))
		line, _ := bufio.NewReader(conn).ReadString('\n')
		if strings.HasPrefix(line, "EHLO ") {
			_, _ = conn.Write([]byte("250 test SMTP\r\n"))
		}
	}()

	sender, err := NewSMTPEmailSender(SMTPConfig{
		Address: listener.Addr().String(), From: "sender@example.com", Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("new sender: %v", err)
	}
	err = sender.SendPasswordReset(context.Background(), "learner@example.com", "https://tutor.example/reset-password?token=secret")
	if err == nil || !strings.Contains(err.Error(), "required STARTTLS") {
		t.Fatalf("send error=%v, want STARTTLS rejection", err)
	}
	<-serverDone
}

func TestSMTPEmailSenderRejectsHeaderInjection(t *testing.T) {
	if _, err := NewSMTPEmailSender(SMTPConfig{
		Address: "smtp.example:587", From: "sender@example.com\r\nBcc: attacker@example.com",
	}); err == nil {
		t.Fatal("header-injected From address accepted")
	}
}
