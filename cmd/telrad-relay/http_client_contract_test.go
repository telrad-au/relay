package main

import (
	"errors"
	"strings"
	"testing"
)

type secretTimeoutError struct{}

func (secretTimeoutError) Error() string   { return "secret request details" }
func (secretTimeoutError) Timeout() bool   { return true }
func (secretTimeoutError) Temporary() bool { return true }

func TestSafeNetworkErrorClassifiesWithoutLeakingDetails(t *testing.T) {
	if safeNetworkError(nil) != nil {
		t.Fatal("nil error was changed")
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"timeout", secretTimeoutError{}, "network_timeout"},
		{"other", errors.New("secret request details"), "network_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := safeNetworkError(test.err)
			if got == nil || got.Error() != test.want {
				t.Fatalf("error = %v, want %s", got, test.want)
			}
			if strings.Contains(got.Error(), "secret") {
				t.Fatalf("safe error leaked details: %v", got)
			}
		})
	}
}

func TestMediaTypeContractRejectsUnexpectedParameters(t *testing.T) {
	tests := []struct {
		value    string
		expected string
		want     bool
	}{
		{"application/json", "application/json", true},
		{"Application/JSON; Charset=UTF-8", "application/json", true},
		{"application/json; charset=utf-8; profile=relay", "application/json", false},
		{"application/hl7-v2", "application/hl7-v2", true},
		{"application/hl7-v2; charset=utf-8", "application/hl7-v2", false},
		{"not a media type", "application/json", false},
	}
	for _, test := range tests {
		if got := mediaTypeEquals(test.value, test.expected); got != test.want {
			t.Errorf("mediaTypeEquals(%q, %q) = %t, want %t", test.value, test.expected, got, test.want)
		}
	}
}

func TestOpaqueIdentifiersAreBoundedPrintableASCII(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"relay-1", true},
		{strings.Repeat("x", 200), true},
		{"", false},
		{strings.Repeat("x", 201), false},
		{"relay id", false},
		{"relay\nsecret", false},
		{"relay-é", false},
	}
	for _, test := range tests {
		if got := validOpaqueID(test.value); got != test.want {
			t.Errorf("validOpaqueID(%q) = %t, want %t", test.value, got, test.want)
		}
	}
}
