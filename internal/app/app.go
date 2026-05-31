package app

import (
	"io"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"
)

type App struct {
	Profiles config.ProfileStore
	Secrets  secret.Store
	Stdout   io.Writer
	Stderr   io.Writer
}

func New(profiles config.ProfileStore, secrets secret.Store, stdout, stderr io.Writer) *App {
	return &App{Profiles: profiles, Secrets: secrets, Stdout: stdout, Stderr: stderr}
}
