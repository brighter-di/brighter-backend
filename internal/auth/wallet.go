package auth

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

var addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

func NormalizeAddress(address string) (string, error) {
	if !addressPattern.MatchString(address) || strings.EqualFold(address, "0x0000000000000000000000000000000000000000") {
		return "", errors.New("Use a valid wallet address.")
	}
	return strings.ToLower(address), nil
}
func keccak(value []byte) []byte { h := sha3.NewLegacyKeccak256(); h.Write(value); return h.Sum(nil) }
func checksum(address string) string {
	lower := strings.TrimPrefix(strings.ToLower(address), "0x")
	hash := hex.EncodeToString(keccak([]byte(lower)))
	result := []byte(lower)
	for i, c := range result {
		if c >= 'a' && c <= 'f' && hash[i] >= '8' {
			result[i] = c - 32
		}
	}
	return "0x" + string(result)
}
func Message(origin, address, nonce string, chainID int64, issuedAt, expiresAt time.Time) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("Invalid login origin.")
	}
	return fmt.Sprintf("%s wants you to sign in with your Ethereum account:\n%s\n\nSign in to Brighter Studio to publish strategies.\n\nURI: %s\nVersion: 1\nChain ID: %d\nNonce: %s\nIssued At: %s\nExpiration Time: %s", u.Host, checksum(address), origin, chainID, nonce, issuedAt.UTC().Format(time.RFC3339), expiresAt.UTC().Format(time.RFC3339)), nil
}
func MessageHash(message string) []byte {
	return keccak([]byte(fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len([]byte(message)), message)))
}
func Verify(message, signature, address string) error {
	expected, err := NormalizeAddress(address)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(signature, "0x") || len(signature) != 132 {
		return errors.New("Invalid wallet signature.")
	}
	raw, err := hex.DecodeString(signature[2:])
	if err != nil {
		return errors.New("Invalid wallet signature.")
	}
	recovery := raw[64]
	if recovery >= 27 {
		recovery -= 27
	}
	if recovery > 1 {
		return errors.New("Invalid wallet signature.")
	}
	compact := append([]byte{27 + recovery}, raw[:64]...)
	pub, _, err := ecdsa.RecoverCompact(compact, MessageHash(message))
	if err != nil {
		return errors.New("Invalid wallet signature.")
	}
	recovered := "0x" + hex.EncodeToString(keccak(pub.SerializeUncompressed()[1:])[12:])
	if recovered != expected {
		return errors.New("The signature does not match this wallet.")
	}
	return nil
}
