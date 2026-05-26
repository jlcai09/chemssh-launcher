package main

import (
	"fmt"
	"os"

	"chemweb-launcher/internal/gui"
	"chemweb-launcher/internal/ui"
	"chemweb-launcher/internal/version"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-v" || os.Args[1] == "version") {
		fmt.Fprintln(os.Stdout, "chemweb-launcher", version.String())
		return
	}

	if len(os.Args) == 1 {
		if err := gui.Run(os.Stdout, os.Stderr); err != nil {
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
