package bootstrap

import (
	"os"
)

func defaultUID() int { return os.Geteuid() }

func defaultOSReader() (string, error) {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
