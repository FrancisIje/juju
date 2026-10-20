package config

import "testing"

func TestSignupIsOpenByDefault(t *testing.T) {
	for _, tc := range []struct {
		env  string // "" means unset
		open bool
	}{
		{"", true},
		{"true", true},
		{"yes", true},
		{"false", false},
		{"FALSE", false},
		{"0", false},
		{"off", false},
	} {
		t.Setenv("JUJU_ALLOW_SIGNUP", tc.env)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.AllowSignup != tc.open {
			t.Errorf("JUJU_ALLOW_SIGNUP=%q: AllowSignup = %v, want %v", tc.env, c.AllowSignup, tc.open)
		}
	}
}
