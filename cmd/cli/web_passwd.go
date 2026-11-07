package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

func initWebPasswdCmd() *cobra.Command {
	var cfgPath string

	cmd := &cobra.Command{
		Use:   "web-passwd",
		Short: "Set or update the Web UI admin password",
		Long: `Sets the password_hash in the ctrld configuration file using bcrypt.
Reads the password interactively from terminal without echoing characters.`,
		Run: func(cmd *cobra.Command, args []string) {
			if cfgPath == "" {
				cfgPath = configPath
			}
			if cfgPath == "" {
				cfgPath = defaultConfigFile
			}
			if cfgPath == "" {
				cfgPath = "/data/controld/ctrld.toml"
			}

			// Read password interactively without echo
			fmt.Print("Enter new Web UI password: ")
			p1, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Println()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error reading password: %v\n", err)
				os.Exit(1)
			}

			passStr := strings.TrimSpace(string(p1))
			if len(passStr) < 8 {
				fmt.Fprintln(os.Stderr, "Error: password must be at least 8 characters long.")
				os.Exit(1)
			}

			fmt.Print("Confirm new Web UI password: ")
			p2, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Println()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error reading confirmation: %v\n", err)
				os.Exit(1)
			}

			if passStr != strings.TrimSpace(string(p2)) {
				fmt.Fprintln(os.Stderr, "Error: passwords do not match.")
				os.Exit(1)
			}

			if err := setWebPasswordInConfigFile(cfgPath, passStr); err != nil {
				fmt.Fprintf(os.Stderr, "Error updating password in config: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Web UI password hash updated successfully in %s\n", cfgPath)
			fmt.Println("Restart or reload ctrld for the change to take effect.")
		},
	}

	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "Path to ctrld config file")
	rootCmd.AddCommand(cmd)
	return cmd
}

func setWebPasswordInConfigFile(cfgPath, passStr string) error {
	// Generate bcrypt hash
	hash, err := bcrypt.GenerateFromPassword([]byte(passStr), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	// Read current config
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("reading config file %s: %w", cfgPath, err)
	}

	// Parse into map to preserve structure and update web.password_hash
	var cfgMap map[string]any
	if err := toml.Unmarshal(data, &cfgMap); err != nil {
		return fmt.Errorf("parsing TOML config %s: %w", cfgPath, err)
	}

	webSection, ok := cfgMap["web"].(map[string]any)
	if !ok {
		webSection = make(map[string]any)
		webSection["enable"] = true
		webSection["listen_ip"] = "192.168.20.1"
		webSection["port"] = int64(5380)
		cfgMap["web"] = webSection
	}
	// Delete obsolete auth_token if present
	delete(webSection, "auth_token")
	webSection["password_hash"] = string(hash)

	// Marshal back to TOML
	outBuf := new(bytes.Buffer)
	if err := toml.NewEncoder(outBuf).Encode(cfgMap); err != nil {
		return fmt.Errorf("serializing TOML config: %w", err)
	}

	// Create timestamped backup
	bakPath := fmt.Sprintf("%s.bak.%s", cfgPath, time.Now().Format("20060102_150405"))
	_ = os.WriteFile(bakPath, data, 0600)

	// Write atomically (.tmp -> rename)
	tmpFile := cfgPath + ".tmp"
	if err := os.WriteFile(tmpFile, outBuf.Bytes(), 0600); err != nil {
		return fmt.Errorf("writing temporary config: %w", err)
	}
	if err := os.Rename(tmpFile, cfgPath); err != nil {
		return fmt.Errorf("applying config file: %w", err)
	}
	return nil
}
