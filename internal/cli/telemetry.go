package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	"github.com/Root-IO-Labs/open-agent-teams/internal/telemetry"
)

// telemetrySetup is `oat telemetry setup` — the zero-to-first-trace flow.
//
// Prompts for Langfuse host (default cloud.langfuse.com), public key, secret
// key. Persists to state.json. Sends a ping span to verify credentials.
// Telemetry is enabled only on a successful ping; a failed ping leaves the
// previous config untouched so a typo doesn't break a working setup.
func (c *CLI) telemetrySetup(args []string) error {
	st, err := state.Load(c.paths.StateFile)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	current := st.GetTelemetry()

	fmt.Println("Set up Langfuse telemetry. Get keys at https://cloud.langfuse.com (free).")
	fmt.Println("Press enter at any prompt to keep the current value.")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	host := promptWithDefault(reader, "Langfuse host", defaultStr(current.Host, "https://cloud.langfuse.com"))
	host = strings.TrimRight(host, "/")
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "https://" + host
	}

	pub := promptWithDefault(reader, "Public key (pk-lf-...)", maskKey(current.PublicKey))
	if pub == maskKey(current.PublicKey) {
		pub = current.PublicKey
	}
	sec := promptWithDefault(reader, "Secret key (sk-lf-...)", maskKey(current.SecretKey))
	if sec == maskKey(current.SecretKey) {
		sec = current.SecretKey
	}

	if pub == "" || sec == "" {
		return fmt.Errorf("public key and secret key are required")
	}

	pending := state.TelemetryConfig{
		Enabled:    true,
		Provider:   "langfuse",
		Host:       host,
		PublicKey:  pub,
		SecretKey:  sec,
		RedactArgs: current.RedactArgs || !current.Enabled, // default-on for first setup
		SampleRate: orDefault(current.SampleRate, 1.0),
		HintShown:  true,
	}

	fmt.Println()
	fmt.Println("Verifying credentials by sending a ping span...")
	if err := telemetryPing(pending); err != nil {
		fmt.Printf("\n✗ Ping failed: %v\n", err)
		fmt.Println("  Your previous config has been kept; no changes saved.")
		fmt.Println("  Common causes: wrong host, expired keys, or no network.")
		return fmt.Errorf("ping failed: %w", err)
	}

	if err := st.SetTelemetry(pending); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	fmt.Println()
	fmt.Println("✓ Telemetry enabled.")
	fmt.Printf("  Host: %s\n", host)
	fmt.Printf("  Public key: %s\n", maskKey(pub))
	fmt.Printf("  Redact args: %v   Sample rate: %.2f\n", pending.RedactArgs, pending.SampleRate)
	fmt.Println()
	fmt.Println("Restart the daemon for in-flight agents to pick up the new config:")
	fmt.Println("  oat daemon restart")
	return nil
}

// telemetryStatus is `oat telemetry status` — read-only summary of current
// config. Keys are masked.
func (c *CLI) telemetryStatus(_ []string) error {
	st, err := state.Load(c.paths.StateFile)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	cfg := st.GetTelemetry()

	if !cfg.Enabled || cfg.PublicKey == "" || cfg.SecretKey == "" {
		fmt.Println("Telemetry: disabled")
		fmt.Println()
		fmt.Println("Enable with: oat telemetry setup")
		return nil
	}

	fmt.Println("Telemetry: enabled (langfuse)")
	fmt.Printf("  Host: %s\n", cfg.Host)
	fmt.Printf("  Public key: %s\n", maskKey(cfg.PublicKey))
	fmt.Printf("  Secret key: %s\n", maskKey(cfg.SecretKey))
	fmt.Printf("  Redact args: %v\n", cfg.RedactArgs)
	fmt.Printf("  Sample rate: %.2f\n", orDefault(cfg.SampleRate, 1.0))
	return nil
}

// telemetryDisable is `oat telemetry disable` — turns off without deleting
// keys, so re-enabling later is one command away.
func (c *CLI) telemetryDisable(_ []string) error {
	st, err := state.Load(c.paths.StateFile)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	cfg := st.GetTelemetry()
	cfg.Enabled = false
	if err := st.SetTelemetry(cfg); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	fmt.Println("Telemetry disabled. Re-enable with: oat telemetry setup")
	return nil
}

// telemetryPing verifies the supplied config can authenticate against Langfuse.
// Synchronous; returns the HTTP/network error verbatim so the user sees a
// useful message ("HTTP 401 — check your keys" / "connection refused").
func telemetryPing(cfg state.TelemetryConfig) error {
	return telemetry.Ping(telemetry.Config{
		Host:      cfg.Host,
		PublicKey: cfg.PublicKey,
		SecretKey: cfg.SecretKey,
		Release:   "oat-setup-ping",
	})
}

// promptWithDefault prints a prompt with the current default in brackets and
// returns the trimmed user input, or the default if the user just pressed
// enter. Empty defaults are shown as "<not set>".
func promptWithDefault(r *bufio.Reader, label, def string) string {
	shown := def
	if shown == "" {
		shown = "<not set>"
	}
	fmt.Printf("%s [%s]: ", label, shown)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// maskKey returns the first 6 and last 4 chars of a key with the middle
// blanked out. Empty string round-trips to empty.
func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 12 {
		return strings.Repeat("*", len(k))
	}
	return k[:6] + strings.Repeat("*", len(k)-10) + k[len(k)-4:]
}

func defaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func orDefault(f, fallback float64) float64 {
	if f <= 0 {
		return fallback
	}
	return f
}
