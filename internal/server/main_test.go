package server

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestMain keeps every test away from the real macOS keychain and Windows
// Credential Manager.
func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}
