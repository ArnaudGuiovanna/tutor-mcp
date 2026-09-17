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

func TestHobbyLoopbackListenDefault(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	if got := httpListenAddress(commandOptions{Profile: "hobby"}, "3000"); got != "127.0.0.1:3000" {
		t.Fatal(got)
	}
	if got := httpListenAddress(commandOptions{}, "3000"); got != ":3000" {
		t.Fatal(got)
	}
	t.Setenv("LISTEN_ADDR", "0.0.0.0:3000")
	if got := httpListenAddress(commandOptions{Profile: "hobby"}, "3000"); got != "0.0.0.0:3000" {
		t.Fatal(got)
	}
}
