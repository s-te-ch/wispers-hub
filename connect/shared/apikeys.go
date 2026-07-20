// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strings"
)

const (
	keyPrefix        = "wc"
	idBytesLen       = 12
	idEncodedLen     = 17
	secretBytesLen   = 32
	secretEncodedLen = 43
)

var (
	errEmptyEnv      = errors.New("apikeys: environment must not be empty")
	errInvalidFormat = errors.New("apikeys: invalid API key format")
	base62Alphabet   = []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
	base62Base       = big.NewInt(62)
	base62Indices    = initBase62Indices()
)

// APIKey contains every derived attribute for a Connect API key.
type APIKey struct {
	ID         string
	Env        string
	Encoded    string
	SecretHash [sha256.Size]byte
}

// Generate creates a new API key for the provided environment using crypto/rand.
func Generate(env string) (*APIKey, error) {
	return generateWithReader(env, rand.Reader)
}

func generateWithReader(env string, r io.Reader) (*APIKey, error) {
	if strings.TrimSpace(env) == "" {
		return nil, errEmptyEnv
	}

	var idBytes [idBytesLen]byte
	if _, err := io.ReadFull(r, idBytes[:]); err != nil {
		return nil, fmt.Errorf("apikeys: read ID bytes: %w", err)
	}
	secretBytes := make([]byte, secretBytesLen)
	if _, err := io.ReadFull(r, secretBytes); err != nil {
		return nil, fmt.Errorf("apikeys: read secret bytes: %w", err)
	}
	defer zero(secretBytes)

	idPart := encodeBase62(idBytes[:], idEncodedLen)
	secretPart := encodeBase62(secretBytes, secretEncodedLen)
	encoded := fmt.Sprintf("%s_%s_%s.%s", keyPrefix, env, idPart, secretPart)

	hash := sha256.Sum256(secretBytes)
	return &APIKey{
		Encoded:    encoded,
		Env:        env,
		ID:         idPart,
		SecretHash: hash,
	}, nil
}

// Errors returned by FromAuthHeader. Their messages are the public HTTP 401
// response bodies, shared verbatim by every implementation of the API.
var (
	ErrNoAuthHeader = errors.New("Authorization header is required")
	// TODO: fix the missing closing quote. Kept verbatim for now so
	// centralising the message didn't change API responses.
	ErrBadAuthHeader = errors.New("Authorization header must be in the format 'Bearer <token>")
	ErrInvalidKey    = errors.New("Invalid API key format")
)

// FromAuthHeader extracts and parses the API key from an HTTP Authorization
// header ("Bearer <key>").
func FromAuthHeader(header string) (*APIKey, error) {
	if header == "" {
		return nil, ErrNoAuthHeader
	}
	parts := strings.Split(header, " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return nil, ErrBadAuthHeader
	}
	key, err := Parse(parts[1])
	if err != nil {
		return nil, ErrInvalidKey
	}
	return key, nil
}

// Parse validates a raw API key string and derives the stored key bytes plus secret hash.
func Parse(raw string) (*APIKey, error) {
	env, idPart, secretPart, err := split(raw)
	if err != nil {
		return nil, err
	}

	secretBytes, err := decodeBase62(secretPart, secretBytesLen)
	if err != nil {
		return nil, fmt.Errorf("apikeys: decode secret: %w", err)
	}
	hash := sha256.Sum256(secretBytes)
	zero(secretBytes)

	return &APIKey{
		ID:         idPart,
		Env:        env,
		Encoded:    raw,
		SecretHash: hash,
	}, nil
}

func split(raw string) (env, idPart, secretPart string, err error) {
	if raw == "" {
		return "", "", "", errInvalidFormat
	}
	if !strings.HasPrefix(raw, keyPrefix+"_") {
		return "", "", "", errInvalidFormat
	}
	trimmed := strings.TrimPrefix(raw, keyPrefix+"_")
	envSep := strings.IndexByte(trimmed, '_')
	if envSep <= 0 {
		return "", "", "", errInvalidFormat
	}
	env = trimmed[:envSep]
	remainder := trimmed[envSep+1:]
	dot := strings.IndexByte(remainder, '.')
	if dot <= 0 || dot == len(remainder)-1 {
		return "", "", "", errInvalidFormat
	}
	idPart = remainder[:dot]
	secretPart = remainder[dot+1:]
	if env == "" || len(idPart) != idEncodedLen || len(secretPart) != secretEncodedLen {
		return "", "", "", errInvalidFormat
	}
	return env, idPart, secretPart, nil
}

func encodeBase62(src []byte, maxLen int) string {
	buf := slices.Repeat([]byte{base62Alphabet[0]}, maxLen)
	num := new(big.Int).SetBytes(src)
	mod := new(big.Int)
	for i := maxLen - 1; i >= 0; i-- {
		if num.Sign() == 0 {
			break
		}
		num.DivMod(num, base62Base, mod)
		buf[i] = base62Alphabet[mod.Int64()]
	}
	if num.Sign() > 0 {
		panic("apikeys: maxLen too small for source bytes")
	}
	return string(buf)
}

func decodeBase62(s string, size int) ([]byte, error) {
	if s == "" {
		return nil, errors.New("apikeys: empty base62 string")
	}
	num := big.NewInt(0)
	for i := 0; i < len(s); i++ {
		val := base62Indices[s[i]]
		if val == 0xFF {
			return nil, fmt.Errorf("apikeys: invalid base62 character %q", s[i])
		}
		num.Mul(num, base62Base)
		num.Add(num, big.NewInt(int64(val)))
	}
	bytes := num.Bytes()
	if len(bytes) > size {
		return nil, fmt.Errorf("apikeys: decoded data exceeds %d bytes", size)
	}
	if len(bytes) < size {
		padding := make([]byte, size-len(bytes))
		bytes = append(padding, bytes...)
	}
	return bytes, nil
}

func initBase62Indices() [256]byte {
	var table [256]byte
	for i := range table {
		table[i] = 0xFF
	}
	for i, ch := range base62Alphabet {
		table[ch] = byte(i)
	}
	return table
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
