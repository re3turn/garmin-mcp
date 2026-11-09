package garmin

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestMain keeps every test away from the real macOS keychain and Windows
// Credential Manager.
func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}

func forgetEncryptionKey() {
	keyCache.Lock()
	keyCache.key = nil
	keyCache.Unlock()
}

// replaceEncryptionKey loses the saved key, as deleting it from the keychain
// would, so files sealed so far can't be opened.
func replaceEncryptionKey(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	if path, err := (fileKeyStore{}).path(); err == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	forgetEncryptionKey()
}

func TestSealedFileHidesItsContent(t *testing.T) {
	setHome(t)
	path := filepath.Join(t.TempDir(), "secret")
	if err := writeSealed(path, sealedAPIKey, []byte("plain-key")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("plain-key")) {
		t.Error("the sealed file holds the plaintext")
	}
	got, err := readSealed(path, sealedAPIKey)
	if err != nil || string(got) != "plain-key" {
		t.Errorf("readSealed = %q, %v", got, err)
	}
}

func TestSealedFileRejectsTampering(t *testing.T) {
	setHome(t)
	sealed, err := seal(sealedAPIKey, []byte("plain-key"))
	if err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(sealed)
	flipped[len(flipped)-1] ^= 1
	for name, tc := range map[string]struct {
		kind string
		data []byte
	}{
		"flipped bit":  {sealedAPIKey, flipped},
		"other kind":   {sealedBearerToken, sealed},
		"truncated":    {sealedAPIKey, sealed[:len(sealMagic)+3]},
		"not sealed":   {sealedAPIKey, []byte("plain-key\n")},
		"empty":        {sealedAPIKey, nil},
		"magic only":   {sealedAPIKey, []byte(sealMagic)},
		"wrong prefix": {sealedAPIKey, append([]byte("GMCP\x02"), sealed[len(sealMagic):]...)},
	} {
		if _, err := unseal(tc.kind, tc.data); !errors.Is(err, errUnsealable) {
			t.Errorf("%s: err = %v, want errUnsealable", name, err)
		}
	}
}

func TestReadingCreatesNoKey(t *testing.T) {
	setHome(t)
	replaceEncryptionKey(t)
	if _, err := unseal(sealedAPIKey, []byte(sealMagic+"0123456789abcdefghijklmnopqrstuvwxyz")); !errors.Is(err, errUnsealable) {
		t.Errorf("err = %v", err)
	}
	if _, err := keys.load(); !errors.Is(err, errNoKey) {
		t.Errorf("a read saved a key: %v", err)
	}
}

// A process that cached a key another process has since replaced reads the
// files sealed with the new key, rather than asking for a sign-in until it
// restarts.
func TestReadingPicksUpAKeyAnotherProcessSaved(t *testing.T) {
	setHome(t)
	if _, err := seal(sealedAPIKey, []byte("old")); err != nil {
		t.Fatal(err)
	}
	keyCache.Lock()
	stale := keyCache.key
	keyCache.Unlock()
	replaceEncryptionKey(t)
	path := filepath.Join(t.TempDir(), "secret")
	if err := writeSealed(path, sealedAPIKey, []byte("new")); err != nil {
		t.Fatal(err)
	}
	keyCache.Lock()
	keyCache.key = stale
	keyCache.Unlock()
	if got, err := readSealed(path, sealedAPIKey); err != nil || string(got) != "new" {
		t.Errorf("readSealed = %q, %v", got, err)
	}
	keyCache.Lock()
	defer keyCache.Unlock()
	if bytes.Equal(keyCache.key, stale) {
		t.Error("the replaced key is still cached")
	}
}

func TestKeyringStoreKeepsTheFirstKey(t *testing.T) {
	keyring.MockInit()
	store := keyringStore{name: "test"}
	if _, err := store.load(); !errors.Is(err, errNoKey) {
		t.Fatalf("load before create: %v", err)
	}
	first := bytes.Repeat([]byte{1}, encryptionKeySize)
	if got, err := store.create(first); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("create = %x, %v", got, err)
	}
	if got, err := store.create(bytes.Repeat([]byte{2}, encryptionKeySize)); err != nil || !bytes.Equal(got, first) {
		t.Errorf("second create = %x, %v; want the first key", got, err)
	}
}

func TestKeyringStoreReportsItsFailure(t *testing.T) {
	keyring.MockInitWithError(errors.New("keychain locked"))
	t.Cleanup(keyring.MockInit)
	if _, err := (keyringStore{name: "test"}).load(); err == nil || errors.Is(err, errNoKey) {
		t.Errorf("err = %v, want the keychain's error", err)
	}
}

func TestFileKeyStoreKeepsTheFirstKeyUnderRace(t *testing.T) {
	setHome(t)
	store := fileKeyStore{}
	results := make([][]byte, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			key, err := store.create(bytes.Repeat([]byte{byte(i + 1)}, encryptionKeySize))
			if err != nil {
				t.Error(err)
			}
			results[i] = key
		})
	}
	wg.Wait()
	for _, key := range results[1:] {
		if !bytes.Equal(key, results[0]) {
			t.Fatalf("keys differ: %x vs %x", key, results[0])
		}
	}
	path, _ := store.path()
	assertPrivate(t, path)
	if filepath.Dir(path) == filepath.Dir(mustConfigFile(t)) {
		t.Error("the key sits next to the files it seals")
	}
}

func mustConfigFile(t *testing.T) string {
	t.Helper()
	path, err := keyFile()
	if err != nil {
		t.Fatal(err)
	}
	return path
}
