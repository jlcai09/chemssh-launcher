package runtime

import (
	"os"

	"chemweb-launcher/internal/config"
	"chemweb-launcher/internal/secret"
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
	if masterPassword := os.Getenv("CHEMWEB_LAUNCHER_VAULT_PASSWORD"); masterPassword != "" {
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
