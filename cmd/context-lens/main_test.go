package main

import "testing"

func TestValidateListenAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		if err := validateListenAddr(addr); err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8080", "192.168.1.10:8080", ":8080"} {
		if err := validateListenAddr(addr); err == nil {
			t.Fatalf("unsafe address accepted: %s", addr)
		}
	}
}
