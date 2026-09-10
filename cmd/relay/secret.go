package main

import (
	"crypto/rand"
	"encoding/hex"
)

func newSecret() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
