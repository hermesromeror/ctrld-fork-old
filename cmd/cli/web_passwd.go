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
				// Windows PowerShell prepends a UTF-8 BOM when piping to a program.
				passStr = strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\uFEFF"))
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
	if _, err := writeConfigBackup(cfgPath, data); err != nil {
		return err
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

// writeConfigBackup saves data as "<cfgPath>.bak.<YYYYMMDD_HHMMSS>" without ever
// overwriting an existing backup: a second backup within the same second gets a
// "_2", "_3"... suffix. It returns the backup path.
func writeConfigBackup(cfgPath string, data []byte) (string, error) {
	base := fmt.Sprintf("%s.bak.%s", cfgPath, time.Now().Format("20060102_150405"))
	for i := 1; i < 100; i++ {
		bakPath := base
		if i > 1 {
			bakPath = fmt.Sprintf("%s_%d", base, i)
		}
		f, err := os.OpenFile(bakPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("creating backup file %s: %w", bakPath, err)
		}
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			_ = os.Remove(bakPath)
			return "", fmt.Errorf("writing backup file %s: %v %v", bakPath, werr, cerr)
		}
		return bakPath, nil
	}
	return "", fmt.Errorf("creating backup file %s: too many backups in the same second", base)
}

// tomlTableName returns the table name of a "[name]" header line (comments
// allowed after it), or "" when the line is not a table header.
func tomlTableName(trimmed string) string {
	if !strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "[[") {
		return ""
	}
	if i := strings.Index(trimmed, "]"); i > 0 {
		rest := strings.TrimSpace(trimmed[i+1:])
		if rest == "" || strings.HasPrefix(rest, "#") {
			return strings.TrimSpace(trimmed[1:i])
		}
	}
	return ""
}

// tomlKey returns the bare key of a "key = value" line, or "".
func tomlKey(trimmed string) string {
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	if i := strings.Index(trimmed, "="); i > 0 {
		return strings.TrimSpace(trimmed[:i])
	}
	return ""
}

// updateTomlPasswordHash sets web.password_hash in content and removes the
// obsolete web.auth_token, changing nothing else: an existing password_hash line
// is replaced in place; otherwise the line is inserted right after the last
// non-blank line of the [web] table (with that line's indentation), and a [web]
// table is appended when there is none. The file's trailing newline is kept.
func updateTomlPasswordHash(content, hash string) string {
	hashLine := func(indent string) string {
		return fmt.Sprintf("%spassword_hash = '%s'", indent, hash)
	}
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines)+6)
	inWeb, webFound, hashSet := false, false, false
	lastWeb := -1 // index in out of the last non-blank line of [web]

	insertInWeb := func() {
		if !inWeb || hashSet || lastWeb < 0 {
			return
		}
		ref := out[lastWeb]
		indent := ref[:len(ref)-len(strings.TrimLeft(ref, " \t"))]
		if tomlTableName(strings.TrimSpace(ref)) != "" {
			indent += "  "
		}
		out = append(out[:lastWeb+1], append([]string{hashLine(indent)}, out[lastWeb+1:]...)...)
		hashSet = true
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		name := tomlTableName(trimmed)
		if name != "" || strings.HasPrefix(trimmed, "[[") {
			insertInWeb()
			inWeb = name == "web"
			if inWeb {
				webFound = true
				out = append(out, line)
				lastWeb = len(out) - 1
				continue
			}
		}
		if inWeb {
			switch tomlKey(trimmed) {
			case "auth_token":
				continue
			case "password_hash":
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				out = append(out, hashLine(indent))
				lastWeb = len(out) - 1
				hashSet = true
				continue
			}
			out = append(out, line)
			if trimmed != "" {
				lastWeb = len(out) - 1
			}
			continue
		}
		out = append(out, line)
	}
	insertInWeb()

	if !webFound {
		trailingNL := len(out) > 0 && out[len(out)-1] == ""
		if trailingNL {
			out = out[:len(out)-1]
		}
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
		out = append(out, "[web]", "  enable = true", "  listen_ip = '192.168.20.1'", "  port = 5380", hashLine("  "))
		if trailingNL {
			out = append(out, "")
		}
	}
	return strings.Join(out, "\n")
}
