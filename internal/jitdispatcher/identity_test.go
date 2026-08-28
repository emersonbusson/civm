package jitdispatcher

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestNewIdentityUsesFullRandomNonce(t *testing.T) {
	identity, err := NewIdentity(bytes.NewReader(bytes.Repeat([]byte{0xab}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	wantNonce := strings.Repeat("ab", 32)
	if identity.Nonce != wantNonce || identity.Label != "civm-jit-"+wantNonce || !validIdentity(identity) {
		t.Fatalf("unexpected identity: %+v", identity)
	}
	if _, err := NewIdentity(bytes.NewReader([]byte("short"))); err == nil {
		t.Fatal("NewIdentity() accepted short randomness")
	}
}

func TestReadTokenAndZero(t *testing.T) {
	input := "test_token_abcdefghijklmnopqrstuvwxyz012345\n"
	token, err := ReadToken(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if string(token) != strings.TrimSuffix(input, "\n") {
		t.Fatalf("token = %q", token)
	}
	Zero(token)
	if !bytes.Equal(token, make([]byte, len(token))) {
		t.Fatal("Zero() did not clear token bytes")
	}
	for _, value := range []string{"short", strings.Repeat("x", maxTokenBytes+1), "test_token_valid_but\nsecond"} {
		if _, err := ReadToken(strings.NewReader(value)); !errors.Is(err, ErrInvalid) {
			t.Errorf("ReadToken(%q) error = %v", value[:min(len(value), 20)], err)
		}
	}
}
