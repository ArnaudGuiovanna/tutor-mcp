package main

import (
	"io"
	"testing"
)

func TestProfileCLI(t *testing.T) {
	for _, args := range [][]string{{}, {"--local"}, {"--local", "--data-dir", "./profile"}, {"--profile", "hobby"}, {"--profile", "institution"}, {"init", "--profile", "hobby", "--public-url", "https://tutor.example.org"}, {"users", "reset", "alice", "--data-dir", "./hobby"}, {"users", "invite"}, {"users", "list"}, {"users", "disable", "alice"}} {
		if _, err := parseCommand(args, io.Discard); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"--local", "--profile", "hobby"}, {"--profile", "invalid"}, {"--data-dir", "x"}, {"users", "reset"}, {"users", "invite", "--profile", "institution"}, {"--public-url", "https://tutor.example.org"}, {"init"}, {"unexpected"}} {
		if _, err := parseCommand(args, io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestLoopbackListenDefault(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	for _, profile := range []string{"hobby", "institution", ""} {
		if got := httpListenAddress(commandOptions{Profile: profile}, "3000"); got != "127.0.0.1:3000" {
			t.Fatalf("profile %q listens on %q, want loopback", profile, got)
		}
	}
	t.Setenv("LISTEN_ADDR", "0.0.0.0:3000")
	for _, profile := range []string{"hobby", ""} {
		if got := httpListenAddress(commandOptions{Profile: profile}, "3000"); got != "0.0.0.0:3000" {
			t.Fatalf("profile %q ignored LISTEN_ADDR: %q", profile, got)
		}
	}
}
