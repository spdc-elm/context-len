package main

import "testing"

func TestValidateListenAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:3001", "[::1]:3001", "localhost:3001"} {
		if err := validateListenAddr(addr); err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:3001", "192.168.1.10:3001", ":3001"} {
		if err := validateListenAddr(addr); err == nil {
			t.Fatalf("unsafe address accepted: %s", addr)
		}
	}
}
