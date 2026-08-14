package session

import (
	"encoding/json"
	"testing"
)

// TestParseWallTime_AcceptsWhatModsActuallyEmit. The capture layer is open, so
// wall_time arrives in whatever shape a mod author could reach. Every form here
// must be recognized, because a form that is not recognized costs the model its
// only signal that an upload is partial.
func TestParseWallTime_AcceptsWhatModsActuallyEmit(t *testing.T) {
	instants := []string{
		`"2024-11-15T14:32:00Z"`,          // the spec's form
		`"2024-11-15T14:32:00.1234567Z"`,  // .NET "o" round-trip, as Stardew emits
		`"2024-11-15T14:32:00+02:00"`,     // a non-UTC offset
		`"2024-11-15T14:32:00-0500"`,      // offset without the colon
		`"2024-11-15T14:32:00"`,           // no zone at all
		`"2024-11-15 14:32:00"`,           // space separator
		`"2024-11-15 14:32:00Z"`,          // space separator, zoned
		`"2024-11-15T14:32"`,              // minute precision
		`"2024-11-15"`,                    // date only
		`"2024/11/15 14:32:00"`,           // slashes
		`"Fri, 15 Nov 2024 14:32:00 UTC"`, // RFC 1123
		`"  2024-11-15T14:32:00Z  "`,      // stray whitespace
	}
	for _, in := range instants {
		if got := parseWallTime(json.RawMessage(in)); got.kind != clockInstant {
			t.Errorf("parseWallTime(%s).kind = %v, want clockInstant", in, got.kind)
		}
	}

	counters := []string{
		`"1483"`,   // quoted integer, as Fallout 4 emits
		`1483`,     // bare integer
		`"1483.5"`, // fractional
		`1483.5`,
		`0`,
		`"0"`,
	}
	for _, in := range counters {
		if got := parseWallTime(json.RawMessage(in)); got.kind != clockCounter {
			t.Errorf("parseWallTime(%s).kind = %v, want clockCounter", in, got.kind)
		}
	}

	// Unrecognized is never an error and never a zero reading — it is simply no
	// information, which callers omit rather than report.
	unknown := []string{
		`""`, `"   "`, `null`, `"not a time"`, `"14:32"`, `true`,
		`{"iso":"2024-11-15T14:32:00Z"}`, `[]`, ``,
	}
	for _, in := range unknown {
		if got := parseWallTime(json.RawMessage(in)); got.kind != clockNone {
			t.Errorf("parseWallTime(%s).kind = %v, want clockNone", in, got.kind)
		}
	}
}

// TestParseWallTime_DifferenceSurvivesFormatVariety: the absolute value of an
// instant is never used, only differences, so an unstated timezone or an odd
// separator must not change the answer.
func TestParseWallTime_DifferenceSurvivesFormatVariety(t *testing.T) {
	cases := []struct {
		name, from, to string
		want           int
	}{
		{"RFC 3339", `"2024-11-15T14:00:00Z"`, `"2024-11-15T14:20:00Z"`, 1200},
		{"no zone", `"2024-11-15T14:00:00"`, `"2024-11-15T14:20:00"`, 1200},
		{"space separator", `"2024-11-15 14:00:00"`, `"2024-11-15 14:20:00"`, 1200},
		{"fractional", `"2024-11-15T14:00:00.500Z"`, `"2024-11-15T14:20:00.500Z"`, 1200},
		{"across an offset", `"2024-11-15T14:00:00+00:00"`, `"2024-11-15T16:20:00+02:00"`, 1200},
		{"quoted counter", `"100"`, `"1300"`, 1200},
		{"bare counter", `100`, `1300`, 1200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := delta(parseWallTime(json.RawMessage(tc.from)), parseWallTime(json.RawMessage(tc.to)))
			if !ok {
				t.Fatalf("delta not measurable for %s → %s", tc.from, tc.to)
			}
			if got != tc.want {
				t.Errorf("delta = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestDelta_DeclinesRatherThanGuesses is the narrow half of the contract. Each
// of these could be turned into a number by guessing; none of them should be.
func TestDelta_DeclinesRatherThanGuesses(t *testing.T) {
	cases := []struct {
		name, from, to string
	}{
		{
			// The whole reason kinds exist: an instant is ~1.7e9 seconds and a
			// counter is ~1e3, so subtracting them yields a confident, enormous,
			// meaningless number.
			"instant minus counter",
			`"2024-11-15T14:00:00Z"`, `"1300"`,
		},
		{"counter minus instant", `"1300"`, `"2024-11-15T14:00:00Z"`},
		{"counter reset", `"5000"`, `"3"`},
		{"instant going backwards", `"2024-11-15T14:20:00Z"`, `"2024-11-15T14:00:00Z"`},
		{"unparseable start", `"not a time"`, `"2024-11-15T14:00:00Z"`},
		{"unparseable end", `"2024-11-15T14:00:00Z"`, `"not a time"`},
		{"both unparseable", `"x"`, `"y"`},
		{"missing start", ``, `"2024-11-15T14:00:00Z"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := delta(parseWallTime(json.RawMessage(tc.from)), parseWallTime(json.RawMessage(tc.to))); ok {
				t.Errorf("delta reported %d; want no measurement", got)
			}
		})
	}
}

// TestParseWallTime_MixedKindsInOneFileDoNotContaminate: a mod that changes
// format partway (or an events file concatenated from two mod versions) must
// degrade to "unknown" across the seam, not to a wrong number — while readings
// on either side of it still measure normally.
func TestParseWallTime_MixedKindsInOneFileDoNotContaminate(t *testing.T) {
	counter := parseWallTime(json.RawMessage(`"100"`))
	later := parseWallTime(json.RawMessage(`"400"`))
	instant := parseWallTime(json.RawMessage(`"2024-11-15T14:00:00Z"`))

	if _, ok := delta(counter, instant); ok {
		t.Error("measured across a format change")
	}
	if got, ok := delta(counter, later); !ok || got != 300 {
		t.Errorf("same-kind delta = %d (ok=%v), want 300", got, ok)
	}
}
