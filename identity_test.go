package main

import (
	"testing"
)

func TestCanonicalSessionID(t *testing.T) {
	orig := bootNonce
	defer func() { bootNonce = orig }()

	s1 := CanonicalSessionID("test_signal")
	s2 := CanonicalSessionID("test_signal")
	if s1 != s2 {
		t.Fatalf("expected identical session within same process, got %s vs %s", s1, s2)
	}
	if !isOfficialSessionID(s1) {
		t.Fatalf("expected official format, got %s", s1)
	}

	bootNonce = []byte("new_nonce_123456")
	s3 := CanonicalSessionID("test_signal")
	if s1 == s3 {
		t.Fatalf("expected different session after nonce change, got %s", s3)
	}
	if !isOfficialSessionID(s3) {
		t.Fatalf("expected official format for s3, got %s", s3)
	}
}

func TestCanonicalSessionIDPreservesOfficial(t *testing.T) {
	official := "ses_e77b800daffe74BrfJXwOsrRv5"
	got := CanonicalSessionID(official)
	if got != official {
		t.Fatalf("expected official session preserved, got %s", got)
	}
}
