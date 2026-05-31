package runtime

import (
	"os"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"
)

type Runtime struct {
	Profiles *config.FileStore
	Secrets  secret.Store
}

func New() (*Runtime, error) {
	profilePath, err := config.DefaultProfilesPath()
	if err != nil {
		return nil, err
	}

	var secrets secret.Store = secret.NewKeyringStore()
	if masterPassword := os.Getenv("CHEMSSH_LAUNCHER_VAULT_PASSWORD"); masterPassword != "" {
		vaultPath, err := config.DefaultVaultPath()
		if err != nil {
			return nil, err
		}
		secrets = secret.NewVaultStore(vaultPath, masterPassword)
	}

	return &Runtime{
		Profiles: config.NewFileStore(profilePath),
		Secrets:  secrets,
	}, nil
}
