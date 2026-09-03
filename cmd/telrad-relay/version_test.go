package main

import "testing"

func TestCompareReleaseVersions(t *testing.T) {
	for _, test := range []struct {
		current   string
		available string
		want      int
	}{
		{current: "2.1.0", available: "2.1.1", want: -1},
		{current: "2.1.0", available: "2.1.0", want: 0},
		{current: "2.1.1", available: "2.1.0", want: 1},
		{current: "2.1.0-rc.1", available: "2.1.0-rc.2", want: -1},
		{current: "2.1.0-rc.2", available: "2.1.0", want: -1},
		{current: "2.1.0", available: "2.1.0-rc.2", want: 1},
	} {
		got, err := compareReleaseVersions(test.current, test.available)
		if err != nil {
			t.Fatalf("compare %s and %s: %v", test.current, test.available, err)
		}
		if got != test.want {
			t.Fatalf("compare %s and %s = %d, want %d", test.current, test.available, got, test.want)
		}
	}
}

func TestParseReleaseVersionRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"", "v2.1.0", "2.1", "02.1.0", "2.1.0+build", "2.1.0-rc.01", "2.1.0-"} {
		if _, err := parseReleaseVersion(value); err == nil {
			t.Fatalf("malformed version %q was accepted", value)
		}
	}
}
