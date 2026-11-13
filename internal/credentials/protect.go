package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

var keyIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Keyring protects only provider cleanup handles. It is not a secret store.
// Keep old keys available until their leases have confirmed cleanup.
type Keyring struct {
	active string
	keys   map[string]cipher.AEAD
}

func NewKeyring(active string, keys map[string][]byte) (*Keyring, error) {
	if !keyIDPattern.MatchString(active) || len(keys) == 0 || len(keys) > 16 {
		return nil, ErrUnavailable
	}
	k := &Keyring{active: active, keys: map[string]cipher.AEAD{}}
	for id, key := range keys {
		if !keyIDPattern.MatchString(id) || len(key) != 32 {
			return nil, ErrUnavailable
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, ErrUnavailable
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, ErrUnavailable
		}
		k.keys[id] = aead
	}
	if k.keys[active] == nil {
		return nil, ErrUnavailable
	}
	return k, nil
}

// LoadKeyring reads an operator-mounted 0600 JSON keyring with base64 keys:
// {"active":"key-1","keys":{"key-1":"<base64 32 bytes>"}}.
func LoadKeyring(path string) (*Keyring, error) {
	b, err := ReadPrivateFile(path, 16384)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer clear(b)
	var cfg struct {
		Active string            `json:"active"`
		Keys   map[string]string `json:"keys"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return nil, ErrUnavailable
	}
	keys := map[string][]byte{}
	defer func() {
		for _, key := range keys {
			clear(key)
		}
	}()
	for id, value := range cfg.Keys {
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, ErrUnavailable
		}
		keys[id] = key
	}
	return NewKeyring(cfg.Active, keys)
}

func handleAAD(r *Record) []byte {
	b, _ := json.Marshal(struct {
		ID                string
		Owner             Owner
		Binding, Revision string
	}{r.ID, r.Owner, r.BindingID, r.BindingRevision})
	return b
}

func (k *Keyring) seal(r *Record, handle string) (string, error) {
	if k == nil || handle == "" {
		return "", ErrUnavailable
	}
	aead := k.keys[k.active]
	if aead == nil {
		return "", ErrUnavailable
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", ErrUnavailable
	}
	sealed := aead.Seal(nonce, nonce, []byte(handle), handleAAD(r))
	return "lease-v1." + k.active + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (k *Keyring) open(r *Record) (string, error) {
	if k == nil {
		return "", ErrUnavailable
	}
	parts := strings.Split(r.ProtectedHandle, ".")
	if len(parts) != 3 || parts[0] != "lease-v1" {
		return "", ErrUnavailable
	}
	aead := k.keys[parts[1]]
	if aead == nil {
		return "", ErrUnavailable
	}
	sealed, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sealed) < aead.NonceSize()+aead.Overhead() {
		return "", ErrUnavailable
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], handleAAD(r))
	if err != nil {
		return "", ErrUnavailable
	}
	defer clear(plain)
	return string(plain), nil
}
