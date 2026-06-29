package updater

import (
	"testing"
)

func TestParseSemver(t *testing.T) {
	cases := []struct {
		input   string
		want    semver
		wantErr bool
	}{
		{"1.2.3", semver{1, 2, 3}, false},
		{"v1.2.3", semver{1, 2, 3}, false},
		// dirty / untagged builds — must be rejected
		{"v1.2.3-4-gabcdef", semver{}, true},
		{"dev", semver{}, true},
		{"1.2.3-beta.1", semver{}, true},
		// malformed
		{"1.2", semver{}, true},
		{"1.2.x", semver{}, true},
	}
	for _, c := range cases {
		got, err := parseSemver(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSemver(%q): want error, got nil", c.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSemver(%q): unexpected error: %v", c.input, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSemver(%q) = %+v, want %+v", c.input, got, c.want)
		}
	}
}

func TestNewerThan(t *testing.T) {
	cases := []struct {
		a, b semver
		want bool
	}{
		{semver{1, 3, 0}, semver{1, 2, 0}, true},
		{semver{2, 0, 0}, semver{1, 9, 9}, true},
		{semver{1, 2, 4}, semver{1, 2, 3}, true},
		{semver{1, 2, 3}, semver{1, 2, 3}, false},
		{semver{1, 2, 2}, semver{1, 2, 3}, false},
		{semver{0, 9, 9}, semver{1, 0, 0}, false},
	}
	for _, c := range cases {
		if got := newerThan(c.a, c.b); got != c.want {
			t.Errorf("newerThan(%+v, %+v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
