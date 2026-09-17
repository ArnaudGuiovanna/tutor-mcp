package main

import (
	"flag"
	"fmt"
	"io"
)

type commandOptions struct {
	Local      bool
	Profile    string
	DataDir    string
	PublicURL  string
	Command    string
	UserAction string
	LoginName  string
}

func parseCommand(args []string, output io.Writer) (commandOptions, error) {
	var options commandOptions
	if len(args) > 0 && (args[0] == "init" || args[0] == "users") {
		options.Command, args = args[0], args[1:]
		if options.Command == "users" && len(args) > 0 {
			options.UserAction, args = args[0], args[1:]
			if (options.UserAction == "reset" || options.UserAction == "disable") && len(args) > 0 {
				options.LoginName, args = args[0], args[1:]
			}
		}
	}
	f := flag.NewFlagSet("tutor-mcp", flag.ContinueOnError)
	f.SetOutput(output)
	f.BoolVar(&options.Local, "local", false, "individual stdio mode; no account, HTTP listener, JWT or SMTP")
	f.StringVar(&options.Profile, "profile", "", "HTTP installation profile: hobby or institution (no option preserves legacy configuration)")
	f.StringVar(&options.DataDir, "data-dir", "", "private data directory (default: ~/.tutor-mcp/local or ~/.tutor-mcp/hobby)")
	f.StringVar(&options.PublicURL, "public-url", "", "canonical HTTPS origin for hobby initialization")
	f.Usage = func() {
		fmt.Fprintln(output, "Usage: tutor-mcp [--local | --profile hobby|institution] [--data-dir PATH]\n       tutor-mcp init --profile hobby --public-url https://your-domain [--data-dir PATH]\n       tutor-mcp users invite|list|reset <identifier>|disable <identifier> [--data-dir PATH]\n       tutor-mcp --version\n\nLocal runs until the MCP client disconnects. Background tasks run while a client is connected.\nLocal and VPS data are independent; profile conversion and synchronization are not automatic.")
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return options, err
	}
	if f.NArg() != 0 {
		return options, fmt.Errorf("unexpected arguments: %v", f.Args())
	}
	if options.Local && (options.Profile != "" || options.Command != "") {
		return options, fmt.Errorf("--local is only available for stdio and cannot be combined with --profile or a subcommand")
	}
	if options.Profile != "" && options.Profile != "hobby" && options.Profile != "institution" {
		return options, fmt.Errorf("--profile must be hobby or institution")
	}
	if options.DataDir != "" && !options.Local && options.Profile != "hobby" && options.Command != "users" {
		return options, fmt.Errorf("--data-dir requires --local or hobby")
	}
	if options.PublicURL != "" && options.Command != "init" {
		return options, fmt.Errorf("--public-url is only available with init")
	}
	if options.Command == "init" && options.Profile != "hobby" {
		return options, fmt.Errorf("init requires --profile hobby")
	}
	if options.Command == "users" {
		if options.Profile == "institution" {
			return options, fmt.Errorf("users commands are only available for hobby")
		}
		options.Profile = "hobby"
		switch options.UserAction {
		case "invite", "list":
		case "reset", "disable":
			if options.LoginName == "" {
				return options, fmt.Errorf("users %s requires an identifier", options.UserAction)
			}
		default:
			return options, fmt.Errorf("users requires invite, list, reset or disable")
		}
	}
	return options, nil
}
