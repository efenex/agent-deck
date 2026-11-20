package tmux

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureThemeConfigWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	original := statusLog
	statusLog = slog.New(slog.NewJSONHandler(&buf, nil))
	themeConfigWarningOnce = sync.Once{}
	t.Cleanup(func() {
		statusLog = original
		themeConfigWarningOnce = sync.Once{}
	})
	return &buf
}

func TestResolvedAgentDeckTheme_InvalidConfig(t *testing.T) {
	for _, location := range []string{"legacy", "xdg"} {
		for _, fixture := range []struct {
			name string
			body string
		}{
			{"syntax", "theme = \"dark\"\ninvalid = [\n"},
			{"duplicate_remote", "theme = \"dark\"\n[remote]\nhost = \"example.invalid\"\n[remote]\nhost = \"example.invalid\"\n"},
			{"duplicate_named_remote", "theme = \"dark\"\n[remotes.example]\nhost = \"example.invalid\"\n[remotes.example]\nhost = \"example.invalid\"\n"},
			{"wrong_type", "theme = 42\n"},
		} {
			t.Run(location+"/"+fixture.name, func(t *testing.T) {
				home := t.TempDir()
				xdg := filepath.Join(home, "xdg")
				t.Setenv("HOME", home)
				t.Setenv("XDG_CONFIG_HOME", xdg)
				t.Setenv("COLORFGBG", "0;15")
				legacyDir := filepath.Join(home, ".agent-deck")
				require.NoError(t, os.MkdirAll(legacyDir, 0700))
				configDir := legacyDir
				if location == "xdg" {
					// Invalid XDG must not fall back to a valid legacy config.
					// Light makes an accidental cross-file fallback observable.
					require.NoError(t, os.WriteFile(filepath.Join(legacyDir, "config.toml"), []byte("theme = \"light\"\n"), 0600))
					configDir = filepath.Join(xdg, "agent-deck")
					require.NoError(t, os.MkdirAll(configDir, 0700))
				}
				require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(fixture.body), 0600))

				logs := captureThemeConfigWarnings(t)
				assert.Equal(t, "dark", resolvedAgentDeckTheme())
				style := currentTmuxThemeStyle()
				assert.Equal(t, "default", style.windowStyle)
				assert.Equal(t, "default", style.windowActiveStyle)
				assert.Equal(t, 1, strings.Count(logs.String(), "\n"), "repeated resolution must warn only once")
				assert.Contains(t, logs.String(), `"level":"WARN"`)
				assert.Contains(t, logs.String(), `"operation":"read_or_decode"`)
				assert.NotContains(t, logs.String(), "example.invalid")
			})
		}
	}
}

func TestResolvedAgentDeckTheme_ConfigErrors(t *testing.T) {
	for _, failure := range []string{"resolve", "read"} {
		t.Run(failure, func(t *testing.T) {
			home := t.TempDir()
			xdg := filepath.Join(home, "xdg")
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", xdg)
			t.Setenv("COLORFGBG", "0;15")
			require.NoError(t, os.MkdirAll(filepath.Join(xdg, "agent-deck"), 0700))
			configPath := filepath.Join(xdg, "agent-deck", "config.toml")
			if failure == "resolve" {
				// A symlink loop makes stat fail even when running as root.
				require.NoError(t, os.Symlink("config.toml", configPath))
			} else {
				// Stat succeeds, but DecodeFile cannot read a directory.
				require.NoError(t, os.Mkdir(configPath, 0700))
			}
			logs := captureThemeConfigWarnings(t)
			assert.Equal(t, "dark", resolvedAgentDeckTheme())
			assert.Equal(t, "dark", resolvedAgentDeckTheme())
			assert.Equal(t, 1, strings.Count(logs.String(), "\n"))
			assert.Contains(t, logs.String(), `"level":"WARN"`)
			operation := "resolve"
			if failure == "read" {
				operation = "read_or_decode"
			}
			assert.Contains(t, logs.String(), `"operation":"`+operation+`"`)
		})
	}
}

func TestResolvedAgentDeckTheme_ValidAndMissingConfig(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		colorfgbg string
		want      string
	}{
		{"missing_light", "", "0;15", "light"},
		{"missing_dark", "", "15;0", "dark"},
		{"empty_theme", "# no explicit theme\n", "0;15", "light"},
		{"explicit_light", "theme = \"light\"\n", "15;0", "light"},
		{"explicit_dark", "theme = \"dark\"\n", "0;15", "dark"},
		{"system_light", "theme = \"system\"\n", "0;15", "light"},
		{"system_dark", "theme = \"system\"\n", "15;0", "dark"},
		{"unknown_theme", "theme = \"unknown\"\n", "0;15", "dark"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			xdg := filepath.Join(home, "xdg")
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", xdg)
			t.Setenv("COLORFGBG", tc.colorfgbg)
			if tc.body != "" {
				configDir := filepath.Join(xdg, "agent-deck")
				require.NoError(t, os.MkdirAll(configDir, 0700))
				require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(tc.body), 0600))
			}
			logs := captureThemeConfigWarnings(t)
			assert.Equal(t, tc.want, resolvedAgentDeckTheme())
			assert.Empty(t, logs.String(), "valid or missing config must not warn")
		})
	}
}
