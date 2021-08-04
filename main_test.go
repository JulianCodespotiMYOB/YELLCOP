package main

import "testing"

func TestCheckMessage(t *testing.T) {
	warn := "WARN"
	fail := "KICK"
	tests := map[string]struct {
		msg  string
		out  string
		kick bool
	}{
		"valid1": {
			"YELL",
			"",
			false,
		},
		"valid2": {
			"YELL YELL",
			"",
			false,
		},
		"valid3": {
			">ELL <ELL",
			"",
			false,
		},
		"quiet1": {
			"talk",
			warn,
			false,
		},
		"quiet2": {
			"talk talk",
			fail,
			true,
		},
		"fromSlack1": {
			"felix has joined the channel",
			"",
			false,
		},
		"url": {
			"THIS HAS A http://example.com",
			"",
			false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := &handler{
				threshold: 1,
				warnings:  []string{warn},
				failures:  []string{fail},
			}
			actual, kick := h.checkMessage(tt.msg)
			if kick != tt.kick {
				t.Errorf("got %t, want %t", kick, tt.kick)
			}
			if actual != tt.out {
				t.Errorf("got %q, want %q", actual, tt.out)
			}

		})
	}
}
