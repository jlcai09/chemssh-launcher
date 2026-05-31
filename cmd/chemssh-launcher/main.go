package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"chemssh-launcher/internal/gui"
	"chemssh-launcher/internal/ui"
	"chemssh-launcher/internal/version"
	"chemssh-launcher/internal/webview"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-v" || os.Args[1] == "version") {
		fmt.Fprintln(os.Stdout, "chemssh-launcher", version.String())
		return
	}

	if options, ok, err := parseGUIOptions(os.Args[1:]); ok {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if err := gui.RunWithOptions(os.Stdout, os.Stderr, options); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if err := ui.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func parseGUIOptions(args []string) (gui.Options, bool, error) {
	options := gui.Options{UseWebView: webview.DefaultEnabled()}
	switch strings.ToLower(os.Getenv("CHEMSSH_LAUNCHER_WEBVIEW")) {
	case "1", "true", "yes", "on":
		options.UseWebView = true
	case "0", "false", "no", "off":
		options.UseWebView = false
	}
	if strings.EqualFold(os.Getenv("CHEMSSH_LAUNCHER_DEVTOOLS"), "1") ||
		strings.EqualFold(os.Getenv("CHEMSSH_LAUNCHER_DEVTOOLS"), "true") {
		options.DevTools = true
		options.UseWebView = true
	}
	if len(args) == 0 {
		return options, true, nil
	}
	for _, arg := range args {
		switch arg {
		case "--webview":
			options.UseWebView = true
		case "--browser":
			options.ForceBrowser = true
			options.UseWebView = false
		case "--devtools":
			options.DevTools = true
			options.UseWebView = true
		default:
			if strings.HasPrefix(arg, "-") {
				return options, true, fmt.Errorf("unknown GUI option %q", arg)
			}
			return gui.Options{}, false, nil
		}
	}
	if options.ForceBrowser && options.DevTools {
		return options, true, errors.New("--devtools is only meaningful with --webview")
	}
	return options, true, nil
}
