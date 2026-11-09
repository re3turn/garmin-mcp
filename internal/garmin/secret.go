package garmin

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// The key and token files are sealed with AES-256-GCM under one key per user.
// On macOS and Windows that key lives in the OS credential store, so a copy of
// ~/.config/garmin (a backup, a synced dotfiles folder) can't be decrypted. On
// Linux, where a headless host has no credential store, it is a file in a
// different directory from the sealed files. Neither protects against a
// process running as the same user, which this server's own unattended token
// refreshes rule out.

// Kinds bind each sealed file to its purpose: they are part of the AAD, so one
// sealed file put in another's place fails to open.
const (
	sealedAPIKey          = "personal-api-key"
	sealedBearerToken     = "personal-api-bearer-token"
	sealedConnectSignIn   = "unofficial-tokens"
	sealMagic             = "GMCP\x01"
	encryptionKeySize     = 32
	keyringService        = "garmin-mcp"
	keyringUser           = "file-encryption-key"
	fileKeyStoreDirectory = "garmin-mcp"
)

// errNoKey reports that no encryption key has been saved yet.
var errNoKey = errors.New("no encryption key has been saved")

// errUnsealable reports a file that isn't sealed, is damaged, or was sealed
// with a key that is gone. The only fix is to save it again.
var errUnsealable = errors.New("the file can't be decrypted")

type keyStore interface {
	// load returns errNoKey when no key has been saved.
	load() ([]byte, error)
	// create saves key unless another key is already saved, and returns the key
	// in effect.
	create(key []byte) ([]byte, error)
	// where names the store in messages.
	where() string
}

var keys = defaultKeyStore()

func defaultKeyStore() keyStore {
	switch runtime.GOOS {
	case "darwin":
		return keyringStore{name: "the macOS keychain"}
	case "windows":
		return keyringStore{name: "Windows Credential Manager"}
	}
	return fileKeyStore{}
}

// keyCache holds the key once loaded, as reading the macOS keychain runs a
// command. A missing key is not cached: `auth` in another process may create it
// while this one runs.
var keyCache struct {
	sync.Mutex
	key []byte
}

func encryptionKey(create bool) ([]byte, error) {
	keyCache.Lock()
	defer keyCache.Unlock()
	if keyCache.key != nil {
		return keyCache.key, nil
	}
	key, err := keys.load()
	if errors.Is(err, errNoKey) && create {
		fresh := make([]byte, encryptionKeySize)
		rand.Read(fresh)
		key, err = keys.create(fresh)
	}
	if err != nil {
		return nil, err
	}
	keyCache.key = key
	return key, nil
}

// reloadEncryptionKey replaces the cached key with the one saved now, which
// another process may have replaced (it lost the race to save the first key,
// or the key was deleted and `auth` saved a new one). A key no longer saved is
// forgotten, so the next save creates one other processes can read.
func reloadEncryptionKey() ([]byte, error) {
	keyCache.Lock()
	defer keyCache.Unlock()
	key, err := keys.load()
	if err != nil {
		if errors.Is(err, errNoKey) {
			keyCache.key = nil
		}
		return nil, err
	}
	keyCache.key = key
	return key, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func seal(kind string, plaintext []byte) ([]byte, error) {
	key, err := encryptionKey(true)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	sealed := append([]byte(sealMagic), nonce...)
	return gcm.Seal(sealed, nonce, plaintext, []byte(sealMagic+kind)), nil
}

func unseal(kind string, data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte(sealMagic)) {
		return nil, errUnsealable
	}
	key, err := encryptionKey(false)
	if errors.Is(err, errNoKey) {
		return nil, errUnsealable
	}
	if err != nil {
		return nil, err
	}
	plaintext, err := open(key, kind, data[len(sealMagic):])
	if !errors.Is(err, errUnsealable) {
		return plaintext, err
	}
	// The cached key may be one another process has since replaced.
	fresh, err := reloadEncryptionKey()
	if errors.Is(err, errNoKey) || (err == nil && bytes.Equal(fresh, key)) {
		return nil, errUnsealable
	}
	if err != nil {
		return nil, err
	}
	return open(fresh, kind, data[len(sealMagic):])
}

// open takes the sealed data after the magic.
func open(key []byte, kind string, data []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errUnsealable
	}
	plaintext, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte(sealMagic+kind))
	if err != nil {
		return nil, errUnsealable
	}
	return plaintext, nil
}

func writeSealed(path, kind string, plaintext []byte) error {
	data, err := seal(kind, plaintext)
	if err != nil {
		return err
	}
	return writePrivateFile(path, data)
}

// readSealed passes os.ErrNotExist through for a file never saved.
func readSealed(path, kind string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return unseal(kind, data)
}

func decodeKey(text string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(key) != encryptionKeySize {
		return nil, errors.New("the saved encryption key is damaged")
	}
	return key, nil
}

// keyringStore keeps the key in the OS credential store. Two processes saving
// the first key at the same moment can each save their own; the reread after
// saving narrows that window but can't close it, and the loser's files then
// need saving again.
type keyringStore struct{ name string }

func (s keyringStore) where() string { return s.name }

func (s keyringStore) load() ([]byte, error) {
	text, err := keyring.Get(keyringService, keyringUser)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, errNoKey
	}
	if err != nil {
		// Such as a locked keychain in an SSH session. Falling back to a key file
		// would leave files sealed with two different keys.
		return nil, fmt.Errorf("couldn't read the encryption key from %s: %w", s.name, err)
	}
	key, err := decodeKey(text)
	if err != nil {
		return nil, fmt.Errorf("%w in %s", err, s.name)
	}
	return key, nil
}

func (s keyringStore) create(key []byte) ([]byte, error) {
	if existing, err := s.load(); !errors.Is(err, errNoKey) {
		return existing, err
	}
	if err := keyring.Set(keyringService, keyringUser, base64.StdEncoding.EncodeToString(key)); err != nil {
		return nil, fmt.Errorf("couldn't save the encryption key to %s: %w", s.name, err)
	}
	return s.load()
}

// fileKeyStore keeps the key in a file outside ~/.config/garmin, so a copy of
// that folder alone can't be decrypted.
type fileKeyStore struct{}

func (fileKeyStore) path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", fileKeyStoreDirectory, "encryption-key"), nil
}

func (s fileKeyStore) where() string {
	path, err := s.path()
	if err != nil {
		return "a key file"
	}
	return path
}

func (s fileKeyStore) load() ([]byte, error) {
	path, err := s.path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoKey
	}
	if err != nil {
		return nil, err
	}
	key, err := decodeKey(string(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, path)
	}
	return key, nil
}

// create writes the key to a temp file and links it into place, which fails if
// a key is already there: another process can never see a half-written key or
// have its key replaced. It needs a filesystem with hard links, as a local home
// directory has.
func (s fileKeyStore) create(key []byte) ([]byte, error) {
	path, err := s.path()
	if err != nil {
		return nil, err
	}
	err = placeFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o700, func(temp, path string) error {
		if err := os.Link(temp, path); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.load()
}
