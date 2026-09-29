// Package secret keeps the AI provider API keys in the operating system
// keychain: Credential Manager on Windows, Keychain on macOS. They never
// reach settings.json, the logs or the web interface.
package secret

import (
	"errors"

	"github.com/zalando/go-keyring"
)

const service = "Probe Desktop"

// Get returns the key stored for a provider, or "" when there is none.
func Get(provider string) (string, error) {
	key, err := keyring.Get(service, provider)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	return key, err
}

// Set stores the key of a provider; an empty key deletes it.
func Set(provider, key string) error {
	if key == "" {
		err := keyring.Delete(service, provider)
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return err
	}
	return keyring.Set(service, provider, key)
}

// Has reports whether a key is stored for a provider.
func Has(provider string) bool {
	key, err := Get(provider)
	return err == nil && key != ""
}
