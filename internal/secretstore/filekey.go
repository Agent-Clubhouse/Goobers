package secretstore

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/platform/safeopen"
)

// fileKeyStore reads operator-provisioned <name>/<version>.pem keys and an
// <name>/active version marker. It never generates or overwrites key material.
type fileKeyStore struct{ directory string }

func (s *fileKeyStore) Wrap(ctx context.Context, ref instance.KeyRef, plaintext []byte) ([]byte, string, error) {
	if err := validateKeyOperation(ctx, ref, plaintext, false); err != nil {
		return nil, "", err
	}
	key, version, err := s.key(ref)
	if err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	ciphertext, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &key.PublicKey, plaintext, nil)
	if err != nil {
		return nil, "", fmt.Errorf("file-key wrap failed")
	}
	return ciphertext, version, nil
}

func (s *fileKeyStore) Unwrap(ctx context.Context, ref instance.KeyRef, ciphertext []byte) ([]byte, error) {
	if err := validateKeyOperation(ctx, ref, ciphertext, true); err != nil {
		return nil, err
	}
	key, _, err := s.key(ref)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plaintext, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("file-key unwrap failed")
	}
	return plaintext, nil
}

func (s *fileKeyStore) key(ref instance.KeyRef) (*rsa.PrivateKey, string, error) {
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return nil, "", fmt.Errorf("file-key: cannot open key directory")
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{".", ref.Name} {
		info, err := root.Lstat(name)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return nil, "", fmt.Errorf("file-key: key directories must be private directories (0700)")
		}
	}
	version := ref.Version
	if version == "" {
		marker, err := readPrivateKeyFile(root, filepath.Join(ref.Name, "active"), 128)
		if err != nil {
			return nil, "", err
		}
		version = strings.TrimSpace(string(marker))
		if !instance.ValidKeyComponent(version) {
			return nil, "", fmt.Errorf("file-key: invalid active version")
		}
	}
	data, err := readPrivateKeyFile(root, filepath.Join(ref.Name, version+".pem"), 16384)
	if err != nil {
		return nil, "", err
	}
	defer clear(data)
	key, err := parsePrivateRSAKey(data)
	return key, version, err
}

func readPrivateKeyFile(root *os.Root, name string, limit int64) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil || !privateRegularFile(before) {
		return nil, fmt.Errorf("file-key: key and active files must be private regular files (0600)")
	}
	file, err := safeopen.OpenRegularInRoot(root, name)
	if err != nil {
		return nil, fmt.Errorf("file-key: cannot open key file")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !privateRegularFile(info) || !os.SameFile(before, info) {
		return nil, fmt.Errorf("file-key: key file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		clear(data)
		return nil, fmt.Errorf("file-key: cannot read key file or size limit exceeded")
	}
	return data, nil
}

func privateRegularFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0
}

func parsePrivateRSAKey(data []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, fmt.Errorf("file-key: expected one RSA private key PEM block")
	}
	defer clear(block.Bytes)
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, _ = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			key, _ = parsed.(*rsa.PrivateKey)
		}
	}
	if key == nil || key.N.BitLen() < 2048 || key.N.BitLen() > 8192 {
		return nil, fmt.Errorf("file-key: expected a 2048 to 8192 bit RSA private key")
	}
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("file-key: invalid RSA private key")
	}
	return key, nil
}
