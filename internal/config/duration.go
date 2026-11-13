package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that reads naturally from TOML, environment
// variables and flags: a bare number means seconds (8, 0.5), anything else
// is a Go duration ("8s", "1m30s", "500ms").
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String formats like time.Duration ("8s", "1m30s", "0s").
func (d Duration) String() string { return time.Duration(d).String() }

// Set parses s (flag.Value).
func (d *Duration) Set(s string) error {
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalText writes the Go duration form, so TOML/JSON show "8s".
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText parses the text form.
func (d *Duration) UnmarshalText(b []byte) error { return d.Set(string(b)) }

// UnmarshalTOML accepts integers and floats (seconds) and strings.
func (d *Duration) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case int64:
		*d = Duration(time.Duration(x) * time.Second)
	case float64:
		*d = Duration(time.Duration(x * float64(time.Second)))
	case string:
		return d.Set(x)
	default:
		return fmt.Errorf("invalid duration %v (%T): use seconds or a string like \"8s\"", v, v)
	}
	if *d < 0 {
		return fmt.Errorf("negative duration %s", d)
	}
	return nil
}

// ParseDuration parses "8" / "0.5" as seconds, else a Go duration string.
// Negative values are rejected.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var v time.Duration
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		v = time.Duration(f * float64(time.Second))
	} else {
		v, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (use seconds like 8 or a duration like 8s, 500ms, 1m)", s)
		}
	}
	if v < 0 {
		return 0, fmt.Errorf("negative duration %q", s)
	}
	return v, nil
}
