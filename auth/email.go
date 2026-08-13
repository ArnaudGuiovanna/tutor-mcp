// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// EmailSender deliberately accepts a complete, short-lived link and never a
// raw token as a separate argument. Implementations must treat the link as a
// credential and must not log it.
type EmailSender interface {
	SendVerification(ctx context.Context, to, link string) error
	SendPasswordReset(ctx context.Context, to, link string) error
}

// LoginChallengeEmailSender is an optional extension used for adaptive sign-in
// verification. Production SMTP implements it; keeping it separate preserves
// compatibility with custom verification/reset senders until they opt in.
type LoginChallengeEmailSender interface {
	SendLoginChallenge(ctx context.Context, to, link string) error
}

type SMTPConfig struct {
	Address    string
	ServerName string
	From       string
	Username   string
	Password   string
	Timeout    time.Duration
}

type SMTPEmailSender struct {
	address    string
	serverName string
	from       string
	username   string
	password   string
	timeout    time.Duration
}

// NewSMTPEmailSender creates a sender that requires STARTTLS. Plaintext SMTP
// and implicit downgrade are rejected so reset capabilities and credentials
// are never placed on an unencrypted connection.
func NewSMTPEmailSender(cfg SMTPConfig) (*SMTPEmailSender, error) {
	cfg.Address = strings.TrimSpace(cfg.Address)
	if cfg.Address == "" {
		return nil, fmt.Errorf("SMTP address is required")
	}
	host, _, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("invalid SMTP address: %w", err)
	}
	serverName := strings.TrimSpace(cfg.ServerName)
	if serverName == "" {
		serverName = host
	}
	if serverName == "" || strings.ContainsAny(serverName, "\r\n") {
		return nil, fmt.Errorf("invalid SMTP server name")
	}
	from, err := mail.ParseAddress(strings.TrimSpace(cfg.From))
	if err != nil || from.Address == "" || strings.ContainsAny(from.Address, "\r\n") {
		return nil, fmt.Errorf("invalid SMTP from address")
	}
	if strings.ContainsAny(cfg.Username, "\r\n") {
		return nil, fmt.Errorf("invalid SMTP username")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &SMTPEmailSender{
		address: cfg.Address, serverName: serverName, from: from.Address,
		username: cfg.Username, password: cfg.Password, timeout: cfg.Timeout,
	}, nil
}

func (s *SMTPEmailSender) SendVerification(ctx context.Context, to, link string) error {
	return s.send(ctx, to, "Verify your tutor/mcp email", "Verify your email to finish connecting tutor/mcp:\n\n"+link+"\n\nThis link expires shortly and can be used only once.\n")
}

func (s *SMTPEmailSender) SendPasswordReset(ctx context.Context, to, link string) error {
	return s.send(ctx, to, "Reset your tutor/mcp password", "Use this link to reset your tutor/mcp password:\n\n"+link+"\n\nThis link expires shortly and can be used only once. If you did not request it, ignore this email.\n")
}

func (s *SMTPEmailSender) SendLoginChallenge(ctx context.Context, to, link string) error {
	return s.send(ctx, to, "Confirm a tutor/mcp sign-in", "A correct password was entered after unusual failed sign-in activity. Confirm this device only if it was you:\n\n"+link+"\n\nThe link expires shortly and can be used only once. If this was not you, reset your password.\n")
}

func (s *SMTPEmailSender) send(ctx context.Context, to, subject, body string) error {
	recipient, err := mail.ParseAddress(strings.TrimSpace(to))
	if err != nil || recipient.Address != to || strings.ContainsAny(to, "\r\n") {
		return fmt.Errorf("invalid recipient address")
	}
	if strings.Contains(body, "\r") {
		return fmt.Errorf("invalid message body")
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", s.address)
	if err != nil {
		return fmt.Errorf("connect SMTP: %w", err)
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, s.serverName)
	if err != nil {
		return fmt.Errorf("start SMTP: %w", err)
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return fmt.Errorf("SMTP server does not support required STARTTLS")
	}
	if err := client.StartTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: s.serverName,
	}); err != nil {
		return fmt.Errorf("secure SMTP: %w", err)
	}
	if s.username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.username, s.password, s.serverName)); err != nil {
			return fmt.Errorf("authenticate SMTP: %w", err)
		}
	}
	if err := client.Mail(s.from); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	if err := client.Rcpt(recipient.Address); err != nil {
		return fmt.Errorf("SMTP RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	message := "From: " + s.from + "\r\n" +
		"To: " + recipient.Address + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\n" +
		strings.ReplaceAll(body, "\n", "\r\n")
	buffered := bufio.NewWriter(w)
	if _, err := buffered.WriteString(message); err != nil {
		_ = w.Close()
		return fmt.Errorf("write SMTP message: %w", err)
	}
	if err := buffered.Flush(); err != nil {
		_ = w.Close()
		return fmt.Errorf("flush SMTP message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish SMTP message: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("quit SMTP: %w", err)
	}
	return nil
}
