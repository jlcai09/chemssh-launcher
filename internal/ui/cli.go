package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"chemssh-launcher/internal/app"
	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/netcheck"
	"chemssh-launcher/internal/runtime"
	"chemssh-launcher/internal/secret"
	"chemssh-launcher/internal/sshclient"
	"chemssh-launcher/internal/version"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func Run(args []string, in io.Reader, out, errOut io.Writer) error {
	if len(args) == 0 {
		printUsage(out)
		return nil
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(out, "chemssh-launcher", version.String())
		return nil
	}

	rt, err := runtime.New()
	if err != nil {
		return err
	}

	switch args[0] {
	case "profile":
		return runProfile(args[1:], in, out, errOut, rt.Profiles, rt.Secrets)
	case "start":
		if len(args) != 2 {
			return errors.New("usage: chemssh-launcher start <profile>")
		}
		return runStart(args[1], in, out, errOut, rt.Profiles, rt.Secrets)
	default:
		printUsage(out)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runProfile(args []string, in io.Reader, out, errOut io.Writer, profiles config.ProfileStore, secrets secret.Store) error {
	if len(args) == 0 {
		printProfileUsage(out)
		return nil
	}
	switch args[0] {
	case "list":
		return listProfiles(out, profiles)
	case "add":
		return addProfile(in, out, profiles, secrets)
	case "edit":
		if len(args) != 2 {
			return errors.New("usage: chemssh-launcher profile edit <profile>")
		}
		return editProfile(args[1], in, out, profiles, secrets)
	case "delete":
		if len(args) != 2 {
			return errors.New("usage: chemssh-launcher profile delete <profile>")
		}
		return deleteProfile(args[1], out, profiles, secrets)
	case "test":
		if len(args) != 2 {
			return errors.New("usage: chemssh-launcher profile test <profile>")
		}
		return testProfile(args[1], in, out, profiles, secrets)
	default:
		printProfileUsage(out)
		return fmt.Errorf("unknown profile command %q", args[0])
	}
}

func listProfiles(out io.Writer, profiles config.ProfileStore) error {
	items, err := profiles.List()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Fprintln(out, "No profiles yet. Run: chemssh-launcher profile add")
		return nil
	}
	for _, p := range items {
		fmt.Fprintf(out, "%s\t%s\t%s@%s:%d\t%s -> %s\n", p.ID, p.Name, p.SSHUser, p.SSHHost, p.SSHPort, p.LocalAddress(), p.RemoteAddress())
	}
	return nil
}

func addProfile(in io.Reader, out io.Writer, profiles config.ProfileStore, secrets secret.Store) error {
	reader := bufio.NewReader(in)
	p := config.NewProfileDefaults()
	id, err := config.NewID()
	if err != nil {
		return err
	}
	p.ID = id

	if err := promptProfile(reader, out, &p, false); err != nil {
		return err
	}
	if err := promptSecrets(reader, out, &p, secrets); err != nil {
		return err
	}
	if err := profiles.Save(p); err != nil {
		return err
	}
	fmt.Fprintf(out, "Saved profile %q (%s)\n", p.Name, p.ID)
	return nil
}

func editProfile(idOrName string, in io.Reader, out io.Writer, profiles config.ProfileStore, secrets secret.Store) error {
	reader := bufio.NewReader(in)
	p, err := profiles.Get(idOrName)
	if err != nil {
		return err
	}
	if err := promptProfile(reader, out, &p, true); err != nil {
		return err
	}
	if err := promptSecrets(reader, out, &p, secrets); err != nil {
		return err
	}
	if err := profiles.Save(p); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated profile %q (%s)\n", p.Name, p.ID)
	return nil
}

func deleteProfile(idOrName string, out io.Writer, profiles config.ProfileStore, secrets secret.Store) error {
	p, err := profiles.Get(idOrName)
	if err != nil {
		return err
	}
	if err := profiles.Delete(p.ID); err != nil {
		return err
	}
	_ = secrets.Delete(p.ID, secret.KeyPassword)
	_ = secrets.Delete(p.ID, secret.KeyPrivatePassphrase)
	fmt.Fprintf(out, "Deleted profile %q (%s)\n", p.Name, p.ID)
	return nil
}

func testProfile(idOrName string, in io.Reader, out io.Writer, profiles config.ProfileStore, secrets secret.Store) error {
	p, err := profiles.Get(idOrName)
	if err != nil {
		return err
	}
	client, err := dialWithHostKeyPrompt(p, in, out, secrets)
	if err != nil {
		return err
	}
	defer client.Close()
	fmt.Fprintln(out, "SSH connection OK")
	if _, err := sshclient.RunCheckPortCommand(client, p, out, out); err != nil {
		return err
	}
	fmt.Fprintf(out, "ChemSSH port check OK: %s\n", p.RemoteAddress())
	return nil
}

func runStart(idOrName string, in io.Reader, out, errOut io.Writer, profiles config.ProfileStore, secrets secret.Store) error {
	p, err := profiles.Get(idOrName)
	if err != nil {
		return err
	}
	if warning := sshclient.PortWarning(p); warning != "" {
		fmt.Fprintln(errOut, "warning:", warning)
	}
	if p.UsesNonLoopbackLocalHost() {
		fmt.Fprintf(errOut, "warning: local bind host %s may expose the tunnel to other machines\n", p.LocalHost)
	}
	if err := reportPort(out, p); err != nil {
		return err
	}
	client, err := dialWithHostKeyPrompt(p, in, out, secrets)
	if err != nil {
		return err
	}
	_ = client.Close()
	fmt.Fprintf(out, "Starting %q. Local URL: %s\n", p.Name, p.BrowserURL())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return app.New(profiles, secrets, out, errOut).Start(ctx, p)
}

func dialWithHostKeyPrompt(p config.Profile, in io.Reader, out io.Writer, secrets secret.Store) (*ssh.Client, error) {
	client, err := sshclient.Dial(p, secrets)
	if err == nil {
		return client, nil
	}
	var hostKeyErr *sshclient.HostKeyVerificationError
	if !errors.As(err, &hostKeyErr) || hostKeyErr.Mismatch {
		return nil, err
	}
	fmt.Fprintf(out, "Unknown SSH host key for %s\n", hostKeyErr.Address)
	fmt.Fprintf(out, "Key: %s %s\n", hostKeyErr.KeyType, hostKeyErr.Fingerprint)
	fmt.Fprintf(out, "Known hosts file: %s\n", hostKeyErr.KnownHostsPath)
	if !promptBool(bufio.NewReader(in), out, "Trust this host key", false) {
		return nil, err
	}
	return sshclient.DialWithHostKeyPolicy(p, secrets, sshclient.HostKeyAcceptNew)
}

func reportPort(out io.Writer, p config.Profile) error {
	ok, err := netcheck.IsPortAvailable(p.LocalHost, p.LocalPort)
	if err != nil {
		return err
	}
	if ok {
		fmt.Fprintf(out, "Local port OK: %s\n", p.LocalAddress())
		return nil
	}
	ports, err := netcheck.SuggestFreePorts(p.LocalHost, p.LocalPort, 3)
	if err != nil {
		return err
	}
	return fmt.Errorf("local port occupied: %s is already in use; suggested local ports: %v", p.LocalAddress(), ports)
}

func promptProfile(reader *bufio.Reader, out io.Writer, p *config.Profile, editing bool) error {
	if editing {
		fmt.Fprintf(out, "Editing profile %q. Press Enter to keep existing values.\n", p.Name)
	}

	p.Name = promptString(reader, out, "Profile name", p.Name)
	p.SSHHost = promptString(reader, out, "SSH host", p.SSHHost)
	p.SSHPort = promptInt(reader, out, "SSH port", p.SSHPort)
	p.SSHUser = promptString(reader, out, "SSH user", p.SSHUser)
	p.AuthMethod = promptChoice(reader, out, "Auth method", p.AuthMethod, []string{config.AuthPassword, config.AuthPrivateKey})
	if p.AuthMethod == config.AuthPrivateKey {
		p.PrivateKeyPath = promptString(reader, out, "Private key path", p.PrivateKeyPath)
	} else {
		p.PrivateKeyPath = ""
	}
	p.RemoteHost = promptString(reader, out, "Remote ChemSSH host", p.RemoteHost)
	p.RemotePort = promptInt(reader, out, "Remote ChemSSH port", p.RemotePort)
	p.LocalHost = promptString(reader, out, "Local bind host", p.LocalHost)
	p.LocalPort = promptInt(reader, out, "Local bind port", p.LocalPort)
	p.LocalURLPath = promptString(reader, out, "Local URL path", p.LocalURLPath)
	p.HealthCheckURL = promptString(reader, out, "Health check URL override", p.HealthCheckURL)
	p.OpenBrowser = promptBool(reader, out, "Open browser", p.OpenBrowser)
	p.PreStartCommands = promptMultiline(reader, out, "Pre-start commands", p.PreStartCommands)
	p.StartCommand = promptMultiline(reader, out, "Start command", p.StartCommand)
	return nil
}

func promptSecrets(reader *bufio.Reader, out io.Writer, p *config.Profile, secrets secret.Store) error {
	switch p.AuthMethod {
	case config.AuthPassword:
		state := "not set"
		if p.HasPassword {
			state = "saved"
		}
		fmt.Fprintf(out, "Password: %s\n", state)
		defaultAction := "keep"
		if !p.HasPassword {
			defaultAction = "replace"
		}
		action := promptChoice(reader, out, "Password action", defaultAction, []string{"keep", "replace", "clear"})
		switch action {
		case "replace":
			value, err := readSecret(out, "New password")
			if err != nil {
				return err
			}
			if err := secrets.Set(p.ID, secret.KeyPassword, value); err != nil {
				return err
			}
			p.HasPassword = true
		case "clear":
			if err := secrets.Delete(p.ID, secret.KeyPassword); err != nil {
				return err
			}
			p.HasPassword = false
		}
		_ = secrets.Delete(p.ID, secret.KeyPrivatePassphrase)
		p.HasPrivateKeyPassphrase = false
	case config.AuthPrivateKey:
		_ = secrets.Delete(p.ID, secret.KeyPassword)
		p.HasPassword = false
		state := "not set"
		if p.HasPrivateKeyPassphrase {
			state = "saved"
		}
		fmt.Fprintf(out, "Private key passphrase: %s\n", state)
		action := promptChoice(reader, out, "Passphrase action", "keep", []string{"keep", "replace", "clear"})
		switch action {
		case "replace":
			value, err := readSecret(out, "New passphrase")
			if err != nil {
				return err
			}
			if err := secrets.Set(p.ID, secret.KeyPrivatePassphrase, value); err != nil {
				return err
			}
			p.HasPrivateKeyPassphrase = true
		case "clear":
			if err := secrets.Delete(p.ID, secret.KeyPrivatePassphrase); err != nil {
				return err
			}
			p.HasPrivateKeyPassphrase = false
		}
	}
	return nil
}

func promptString(reader *bufio.Reader, out io.Writer, label, current string) string {
	fmt.Fprintf(out, "%s [%s]: ", label, current)
	value := readLine(reader)
	if value == "" {
		return current
	}
	return value
}

func promptInt(reader *bufio.Reader, out io.Writer, label string, current int) int {
	for {
		value := promptString(reader, out, label, strconv.Itoa(current))
		parsed, err := strconv.Atoi(value)
		if err == nil {
			return parsed
		}
		fmt.Fprintln(out, "Please enter a number.")
	}
}

func promptBool(reader *bufio.Reader, out io.Writer, label string, current bool) bool {
	def := "y"
	if !current {
		def = "n"
	}
	for {
		value := strings.ToLower(promptString(reader, out, label+" (y/n)", def))
		switch value {
		case "y", "yes", "true":
			return true
		case "n", "no", "false":
			return false
		}
		fmt.Fprintln(out, "Please enter y or n.")
	}
}

func promptChoice(reader *bufio.Reader, out io.Writer, label, current string, choices []string) string {
	for {
		value := promptString(reader, out, label+" "+strings.Join(choices, "/"), current)
		for _, choice := range choices {
			if value == choice {
				return value
			}
		}
		fmt.Fprintf(out, "Please choose one of: %s\n", strings.Join(choices, ", "))
	}
}

func promptMultiline(reader *bufio.Reader, out io.Writer, label, current string) string {
	fmt.Fprintf(out, "%s. End with a single dot on its own line. Leave empty to keep current/default.\n", label)
	if current != "" {
		fmt.Fprintln(out, "--- current/default ---")
		fmt.Fprintln(out, current)
		fmt.Fprintln(out, "-----------------------")
	}
	var lines []string
	for {
		line := readLine(reader)
		if line == "." {
			break
		}
		lines = append(lines, line)
	}
	value := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if strings.TrimSpace(value) == "" {
		return current
	}
	return value
}

func readSecret(out io.Writer, label string) (string, error) {
	fmt.Fprintf(out, "%s: ", label)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	reader := bufio.NewReader(os.Stdin)
	return readLine(reader), nil
}

func readLine(reader *bufio.Reader) string {
	value, _ := reader.ReadString('\n')
	return strings.TrimRight(value, "\r\n")
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  chemssh-launcher --version")
	fmt.Fprintln(out, "  chemssh-launcher profile <list|add|edit|delete|test>")
	fmt.Fprintln(out, "  chemssh-launcher start <profile>")
}

func printProfileUsage(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  chemssh-launcher profile list")
	fmt.Fprintln(out, "  chemssh-launcher profile add")
	fmt.Fprintln(out, "  chemssh-launcher profile edit <profile>")
	fmt.Fprintln(out, "  chemssh-launcher profile delete <profile>")
	fmt.Fprintln(out, "  chemssh-launcher profile test <profile>")
}
