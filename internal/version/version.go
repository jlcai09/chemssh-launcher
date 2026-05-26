package version

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var rawVersion string

func String() string {
	v := strings.TrimSpace(rawVersion)
	if v == "" {
		return "dev"
	}
	return v
}
