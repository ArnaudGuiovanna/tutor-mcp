package auth

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
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
	if _, err := NewSMTPEmailSender(SMTPConfig{
		Address: "smtp.example:587", ServerName: "smtp.example\r\nX-Injected: yes", From: "sender@example.com",
	}); err == nil {
		t.Fatal("header-injected server name accepted")
	}
	if _, err := NewSMTPEmailSender(SMTPConfig{
		Address: "smtp.example:587", From: "sender@example.com", Username: "user\r\nAUTH PLAIN bad",
	}); err == nil {
		t.Fatal("command-injected SMTP username accepted")
	}
}

func TestSMTPEmailSenderRejectsRecipientAndLinkInjection(t *testing.T) {
	sender, err := NewSMTPEmailSender(SMTPConfig{
		Address: "smtp.example:587", From: "sender@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, recipient := range []string{
		"victim@example.com\r\nBcc: attacker@example.com",
		"victim@example.com\nBcc: attacker@example.com",
		"Victim <victim@example.com>",
		" victim@example.com",
	} {
		if err := sender.SendVerification(context.Background(), recipient, "https://tutor.example/verify?token=test"); err == nil || !strings.Contains(err.Error(), "recipient") {
			t.Errorf("recipient %q error = %v, want rejection", recipient, err)
		}
	}

	for _, link := range []string{
		"https://tutor.example/reset?token=test\r\nBcc: attacker@example.com",
		"https://tutor.example/reset?token=test\nspoofed content",
		"javascript:alert(1)",
		"/reset?token=test",
		"https://user@tutor.example/reset?token=test",
		"https://tutor.example/reset?token=test#fragment",
		" https://tutor.example/reset?token=test",
		strings.Repeat("x", emailLinkMaxLen+1),
	} {
		if err := sender.SendPasswordReset(context.Background(), "victim@example.com", link); err == nil || !strings.Contains(err.Error(), "email link") {
			t.Errorf("link %q error = %v, want rejection", link, err)
		}
	}
}

func TestSMTPEmailSenderKeepsRecipientOutOfMessageHeaders(t *testing.T) {
	listener, roots, serverErr, envelopeRecipient, deliveredMessage := startTLSTestSMTP(t)
	sender, err := NewSMTPEmailSender(SMTPConfig{
		Address: listener.Addr().String(), ServerName: "smtp.test", From: "sender@example.com", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	sender.tlsConfig.RootCAs = roots

	const recipient = "learner@example.com"
	const link = "https://tutor.example/reset-password?token=short-lived-test-token"
	if err := sender.SendPasswordReset(context.Background(), recipient, link); err != nil {
		t.Fatalf("send password reset: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("SMTP server: %v", err)
	}
	if got := <-envelopeRecipient; got != "<"+recipient+">" {
		t.Fatalf("envelope recipient = %q", got)
	}
	message := <-deliveredMessage
	headers, _, found := strings.Cut(message, "\r\n\r\n")
	if !found {
		t.Fatalf("message has no header/body separator: %q", message)
	}
	if strings.Contains(headers, recipient) {
		t.Fatalf("recipient leaked into message headers: %q", headers)
	}
	if !strings.Contains(headers, "To: undisclosed-recipients:;") {
		t.Fatalf("constant To header missing: %q", headers)
	}
	if !strings.Contains(message, link) {
		t.Fatal("validated reset link missing from message body")
	}
}

func startTLSTestSMTP(t *testing.T) (net.Listener, *x509.CertPool, <-chan error, <-chan string, <-chan string) {
	t.Helper()
	certificate, roots := testSMTPCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverErr := make(chan error, 1)
	envelopeRecipient := make(chan string, 1)
	deliveredMessage := make(chan string, 1)
	go func() {
		serverErr <- serveTLSTestSMTP(listener, certificate, envelopeRecipient, deliveredMessage)
	}()
	return listener, roots, serverErr, envelopeRecipient, deliveredMessage
}

func serveTLSTestSMTP(listener net.Listener, certificate tls.Certificate, envelopeRecipient chan<- string, deliveredMessage chan<- string) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, "220 smtp.test ESMTP\r\n"); err != nil {
		return err
	}
	if err := expectSMTPCommand(reader, "EHLO "); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "250-smtp.test\r\n250 STARTTLS\r\n"); err != nil {
		return err
	}
	if err := expectSMTPCommand(reader, "STARTTLS"); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "220 ready for TLS\r\n"); err != nil {
		return err
	}
	tlsConn := tls.Server(conn, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	reader = bufio.NewReader(tlsConn)
	if err := expectSMTPCommand(reader, "EHLO "); err != nil {
		return err
	}
	if _, err := io.WriteString(tlsConn, "250 smtp.test\r\n"); err != nil {
		return err
	}
	if err := expectSMTPCommand(reader, "MAIL FROM:"); err != nil {
		return err
	}
	if _, err := io.WriteString(tlsConn, "250 sender accepted\r\n"); err != nil {
		return err
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "RCPT TO:") {
		return fmt.Errorf("SMTP command %q, want RCPT TO", strings.TrimSpace(line))
	}
	envelopeRecipient <- strings.TrimSpace(strings.TrimPrefix(line, "RCPT TO:"))
	if _, err := io.WriteString(tlsConn, "250 recipient accepted\r\n"); err != nil {
		return err
	}
	if err := expectSMTPCommand(reader, "DATA"); err != nil {
		return err
	}
	if _, err := io.WriteString(tlsConn, "354 send message\r\n"); err != nil {
		return err
	}
	var message strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line == ".\r\n" {
			break
		}
		message.WriteString(line)
	}
	deliveredMessage <- message.String()
	if _, err := io.WriteString(tlsConn, "250 queued\r\n"); err != nil {
		return err
	}
	if err := expectSMTPCommand(reader, "QUIT"); err != nil {
		return err
	}
	_, err = io.WriteString(tlsConn, "221 bye\r\n")
	return err
}

func expectSMTPCommand(reader *bufio.Reader, prefix string) error {
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, prefix) {
		return fmt.Errorf("SMTP command %q, want prefix %q", strings.TrimSpace(line), prefix)
	}
	return nil
}

func testSMTPCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "smtp.test"},
		DNSNames:     []string{"smtp.test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("append SMTP test root certificate")
	}
	return certificate, roots
}
