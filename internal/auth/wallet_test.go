package auth

import (
	"strings"
	"testing"
	"time"
)

// Independently signed by viem/accounts, rather than the Go recovery library.
func TestViemLoginSignature(t *testing.T) {
	address := "0x19E7E376E7C213B7E7e7e46cc70A5dD086DAff2A"
	message := "Brighter wallet login verification fixture"
	signature := "0x6aba8068b77515db99738ad6019fd702b42e62998b0075e08b5c82b28e89a01f2937b0703ff843c119da1bb468b84a7866c6ecb3e2c967ae9713606d3e1044921c"
	if err := Verify(message, signature, address); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ message, signature, address string }{{message + "!", signature, address}, {message, signature, "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, {message, "0x00", address}, {message, signature[:130] + "ff", address}} {
		if Verify(test.message, test.signature, test.address) == nil {
			t.Fatal("accepted a forged signature")
		}
	}
	if err := Verify(message, signature[:130]+"01", strings.ToLower(address)); err != nil {
		t.Fatal("recovery id 0/1:", err)
	}
}
func TestSIWEMessage(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	message, err := Message("https://studio.brighter.fi", "0x19e7e376e7c213b7e7e7e46cc70a5dd086daff2a", "abcd12345678", 56, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"studio.brighter.fi wants you to sign in", "0x19E7E376E7C213B7E7e7e46cc70A5dD086DAff2A", "URI: https://studio.brighter.fi", "Chain ID: 56", "Nonce: abcd12345678", "Expiration Time: 2026-09-13T13:00:00Z"} {
		if !strings.Contains(message, part) {
			t.Fatal("missing", part)
		}
	}
	if _, err = Message("https://evil.example/@studio.brighter.fi", "0x19e7e376e7c213b7e7e7e46cc70a5dd086daff2a", "nonce", 56, now, now); err == nil {
		t.Fatal("accepted invalid origin")
	}
}
