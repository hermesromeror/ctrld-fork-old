package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

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
Reads the password interactively from terminal without echoing characters,
or from stdin if input is piped non-interactively.`,
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

			var passStr string
			if term.IsTerminal(int(os.Stdin.Fd())) {
				fmt.Print("Enter new Web UI password: ")
				p1, err := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Println()
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error reading password: %v\n", err)
					os.Exit(1)
				}
				passStr = strings.TrimSpace(string(p1))
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
			} else {
				// Non-interactive (pipe / redirected stdin)
				scanner := bufio.NewScanner(os.Stdin)
				if !scanner.Scan() {
					fmt.Fprintln(os.Stderr, "Error: failed to read password from stdin")
					os.Exit(1)
				}
				passStr = strings.TrimSpace(scanner.Text())
				if len(passStr) < 8 {
					fmt.Fprintln(os.Stderr, "Error: password must be at least 8 characters long.")
					os.Exit(1)
				}
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

	// Create timestamped backup BEFORE modifying
	bakPath := fmt.Sprintf("%s.bak.%s", cfgPath, time.Now().Format("20060102_150405"))
	if err := os.WriteFile(bakPath, data, 0600); err != nil {
		return fmt.Errorf("creating backup file %s: %w", bakPath, err)
	}

	// Perform surgical textual replacement on [web] section to preserve formatting,
	// comments, indentation, and structure of the rest of the configuration.
	updatedContent := updateTomlPasswordHash(string(data), string(hash))

	// Write atomically (.tmp -> rename)
	tmpFile := cfgPath + ".tmp"
	if err := os.WriteFile(tmpFile, []byte(updatedContent), 0600); err != nil {
		return fmt.Errorf("writing temporary config: %w", err)
	}
	if err := os.Rename(tmpFile, cfgPath); err != nil {
		return fmt.Errorf("applying config file: %w", err)
	}
	return nil
}

func updateTomlPasswordHash(content, hash string) string {
	lines := strings.Split(content, "\n")
	var newLines []string
	inWeb := false
	webFound := false
	hashSet := false

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Check section header
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inWeb && !hashSet {
				// Reached end of [web] without finding password_hash, insert it before next section
				newLines = append(newLines, fmt.Sprintf("  password_hash = '%s'", hash))
				hashSet = true
			}
			if trimmed == "[web]" {
				inWeb = true
				webFound = true
				newLines = append(newLines, line)
				continue
			} else {
				inWeb = false
			}
		}

		if inWeb {
			// Remove obsolete auth_token
			if strings.HasPrefix(trimmed, "auth_token") {
				continue
			}
			// Replace existing password_hash
			if strings.HasPrefix(trimmed, "password_hash") {
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				newLines = append(newLines, fmt.Sprintf("%spassword_hash = '%s'", indent, hash))
				hashSet = true
				continue
			}
		}

		newLines = append(newLines, line)
	}

	if inWeb && !hashSet {
		// File ended while in [web]
		newLines = append(newLines, fmt.Sprintf("  password_hash = '%s'", hash))
		hashSet = true
	}

	if !webFound {
		// Append [web] section if it did not exist
		if len(newLines) > 0 && newLines[len(newLines)-1] != "" {
			newLines = append(newLines, "")
		}
		newLines = append(newLines, "[web]")
		newLines = append(newLines, "  enable = true")
		newLines = append(newLines, "  listen_ip = '192.168.20.1'")
		newLines = append(newLines, "  port = 5380")
		newLines = append(newLines, fmt.Sprintf("  password_hash = '%s'", hash))
	}

	return strings.Join(newLines, "\n")
}
