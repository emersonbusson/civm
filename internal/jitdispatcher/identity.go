package jitdispatcher

import (
	"encoding/hex"
	"fmt"
	"io"
)

const maxTokenBytes = 8192

func NewIdentity(random io.Reader) (Identity, error) {
	value := make([]byte, 32)
	if _, err := io.ReadFull(random, value); err != nil {
		return Identity{}, fmt.Errorf("identity randomness: %w", err)
	}
	nonce := hex.EncodeToString(value)
	return Identity{
		Nonce:      nonce,
		Label:      "civm-jit-" + nonce,
		RunnerName: "civm-jit-" + nonce[:16],
		WorkFolder: "_work/jit-" + nonce,
	}, nil
}

func ReadToken(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxTokenBytes+1))
	if err != nil {
		Zero(data)
		return nil, fmt.Errorf("token read: %w", err)
	}
	if len(data) > maxTokenBytes {
		Zero(data)
		return nil, fmt.Errorf("%w: token is too large", ErrInvalid)
	}
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
		if len(data) > 0 && data[len(data)-1] == '\r' {
			data = data[:len(data)-1]
		}
	}
	if len(data) < 20 {
		Zero(data)
		return nil, fmt.Errorf("%w: token is too short", ErrInvalid)
	}
	for _, char := range data {
		if char <= 0x20 || char >= 0x7f {
			Zero(data)
			return nil, fmt.Errorf("%w: token contains whitespace or non-ASCII data", ErrInvalid)
		}
	}
	return data, nil
}

func Zero(secret []byte) {
	for index := range secret {
		secret[index] = 0
	}
}
