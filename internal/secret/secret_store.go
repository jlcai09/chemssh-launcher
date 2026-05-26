package secret

const (
	ServiceName          = "chemweb-launcher"
	KeyPassword          = "password"
	KeyPrivatePassphrase = "key-passphrase"
)

type Store interface {
	Set(profileID, key, value string) error
	Get(profileID, key string) (string, bool, error)
	Delete(profileID, key string) error
}

func Username(profileID, key string) string {
	return profileID + ":" + key
}
