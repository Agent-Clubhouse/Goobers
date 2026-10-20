package interactiveagent

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net"
	"os"
	"time"
)

const tokenLineSize = 44 // 32 random bytes, unpadded base64url, and a newline.
const authenticationTimeout = 5 * time.Second

func createToken(path string) ([]byte, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, ErrUnavailable
	}
	token := append([]byte(base64.RawURLEncoding.EncodeToString(random[:])), '\n')
	if err := os.WriteFile(path, token, 0600); err != nil {
		return nil, ErrUnavailable
	}
	return token, nil
}

func readToken(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != tokenLineSize {
		return nil, ErrUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0600 {
		return nil, ErrUnavailable
	}
	token, err := io.ReadAll(io.LimitReader(file, tokenLineSize+1))
	if err != nil || len(token) != tokenLineSize || token[tokenLineSize-1] != '\n' {
		return nil, ErrUnavailable
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(string(token[:tokenLineSize-1]))
	if err != nil || len(decoded) != 32 {
		return nil, ErrUnavailable
	}
	return token, nil
}

func (b *Bridge) authenticate(conn net.Conn) bool {
	if conn.SetDeadline(time.Now().Add(authenticationTimeout)) != nil {
		return false
	}
	var supplied [tokenLineSize]byte
	if _, err := io.ReadFull(conn, supplied[:]); err != nil || subtle.ConstantTimeCompare(supplied[:], b.token) != 1 {
		return false
	}
	if _, err := io.WriteString(conn, "OK\n"); err != nil {
		return false
	}
	return conn.SetDeadline(time.Time{}) == nil
}

func authenticateClient(conn net.Conn, token []byte) error {
	if conn.SetDeadline(time.Now().Add(authenticationTimeout)) != nil {
		return ErrUnavailable
	}
	if _, err := conn.Write(token); err != nil {
		return ErrUnavailable
	}
	var reply [3]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil || string(reply[:]) != "OK\n" {
		return ErrUnavailable
	}
	if conn.SetDeadline(time.Time{}) != nil {
		return ErrUnavailable
	}
	return nil
}
