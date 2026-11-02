package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
	"os"
)

type Vault struct{ aead cipher.AEAD }

func Open(path string) (*Vault, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		_, writeErr := f.Write(key)
		syncErr := f.Sync()
		closeErr := f.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || len(key) != 32 {
		return nil, errors.New("master key must be a 32-byte regular file accessible only by its owner")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead}, nil
}

func (v *Vault) Seal(plain []byte, purpose string) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return v.aead.Seal(nonce, nonce, plain, []byte(purpose)), nil
}

func (v *Vault) Open(value []byte, purpose string) ([]byte, error) {
	n := v.aead.NonceSize()
	if len(value) < n {
		return nil, errors.New("invalid encrypted credential")
	}
	return v.aead.Open(nil, value[:n], value[n:], []byte(purpose))
}
