package session

import (
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestRedeliverMaxAge(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"", time.Hour}, {"30m", 30 * time.Minute}, {"2h", 2 * time.Hour}, {"0", 0}, {"-1h", 0}, {"unlimited", 0}} {
		var cfg UserConfig
		if _, err := toml.Decode("[send]\nredeliver_max_age = '"+tc.value+"'\n", &cfg); err != nil {
			t.Fatal(err)
		}
		if got := cfg.GetRedeliverMaxAge(); got != tc.want {
			t.Errorf("%q: %s, want %s", tc.value, got, tc.want)
		}
	}
	var nilConfig *UserConfig
	if nilConfig.GetRedeliverMaxAge() != time.Hour {
		t.Fatal("nil config default")
	}
}
