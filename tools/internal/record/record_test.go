package record

import (
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	good := map[string]int64{
		"768M": 768 << 20,
		"1G":   1 << 30,
		"64k":  64 << 10,
		"4096": 4096,
	}
	for s, want := range good {
		if got, err := ParseSize(s); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v, want %d", s, got, err, want)
		}
	}
	for _, s := range []string{"", "M", "-1G", "1.5G", "1GB", "0", "12x", "99999999999G"} {
		if _, err := ParseSize(s); err == nil {
			t.Errorf("ParseSize(%q) succeeded", s)
		}
	}
}

func TestCheckName(t *testing.T) {
	for _, name := range []string{"wlan-06-00-0", "net0", "a"} {
		if err := CheckName(name); err != nil {
			t.Errorf("CheckName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "control", "invirtio", "devices", "-x", "Wlan", "a/b", "a.b", "a,b",
		strings.Repeat("a", 33)} {
		if CheckName(name) == nil {
			t.Errorf("CheckName(%q) succeeded", name)
		}
	}
}
