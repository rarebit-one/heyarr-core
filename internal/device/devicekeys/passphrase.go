package devicekeys

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/rarebit-one/void-which-binds-go/custody"
	"golang.org/x/term"
)

// PassphraseFileEnvVar names a file holding a custody device's passphrase, for
// a process with no terminal to ask on (an agent-launched Personal MCP, a
// gateway under a service manager, a test). Without it the passphrase is asked
// for on the terminal, never taken from an argument or the environment itself:
// a secret on the command line is in shell history and every user's `ps`.
const PassphraseFileEnvVar = "HEYARR_DEVICE_PASSPHRASE_FILE" // #nosec G101 -- the name of a variable, not a credential

// ErrNoTerminal is a passphrase that has to be asked for with no terminal to
// ask on and no passphrase file.
var ErrNoTerminal = errors.New("devicekeys: no terminal to ask for the device passphrase on")

// ErrPassphraseMismatch is a new passphrase whose confirmation differs.
var ErrPassphraseMismatch = errors.New("devicekeys: the two passphrases differ")

// MinPassphraseLen is the shortest passphrase a new sealed file is sealed
// under, in Unicode code points. A sealed file can be guessed offline, slowed
// only by its Argon2id cost, so the passphrase is the whole of its strength.
// It is checked when sealing only: an existing file opens with whatever it was
// sealed under.
const MinPassphraseLen = 12

// ErrPassphraseTooShort is a new passphrase shorter than MinPassphraseLen.
var ErrPassphraseTooShort = fmt.Errorf("devicekeys: a new passphrase must be at least %d characters", MinPassphraseLen)

// DefaultPIN is the passphrase source for the custody device whose sealed file
// is at path: the file PassphraseFileEnvVar names, else a prompt on the
// terminal, without echo. It reads nothing until it is called.
func DefaultPIN(path string) custody.PINFunc {
	return func() (string, error) {
		if file := os.Getenv(PassphraseFileEnvVar); file != "" {
			return ReadPassphraseFile(file, nil)
		}
		return readTerminal(fmt.Sprintf("Passphrase for this device's sealed keys (%s): ", path))
	}
}

// NewPassphrase is the passphrase source for sealing a new custody device: the
// file (or "-", stdin) passed as file, else the file PassphraseFileEnvVar
// names, else a prompt on the terminal, asked twice and compared. A file is
// taken as written, so it is not confirmed. A passphrase shorter than
// MinPassphraseLen code points is ErrPassphraseTooShort.
func NewPassphrase(file string, stdin io.Reader) custody.PINFunc {
	return func() (string, error) {
		if file == "" {
			file = os.Getenv(PassphraseFileEnvVar)
		}
		if file != "" {
			pass, err := ReadPassphraseFile(file, stdin)
			if err != nil {
				return "", err
			}
			return pass, checkNewPassphrase(pass)
		}
		first, err := readTerminal(fmt.Sprintf("New passphrase for this device's sealed keys (at least %d characters): ", MinPassphraseLen))
		if err != nil {
			return "", err
		}
		if err := checkNewPassphrase(first); err != nil {
			return "", err
		}
		second, err := readTerminal("Repeat the passphrase: ")
		if err != nil {
			return "", err
		}
		if first != second {
			return "", ErrPassphraseMismatch
		}
		return first, nil
	}
}

// checkNewPassphrase refuses a passphrase too short to seal a new file under.
func checkNewPassphrase(pass string) error {
	if n := utf8.RuneCountInString(pass); n < MinPassphraseLen {
		return fmt.Errorf("%w (this one is %d)", ErrPassphraseTooShort, n)
	}
	return nil
}

// ReadPassphraseFile reads a passphrase from its first line: of file, or of
// stdin when file is "-". Only the line ending is removed, so a passphrase may
// begin or end with spaces.
func ReadPassphraseFile(file string, stdin io.Reader) (string, error) {
	var r io.Reader
	if file == "-" {
		if stdin == nil {
			stdin = os.Stdin
		}
		r = stdin
	} else {
		f, err := os.Open(filepath.Clean(file))
		if err != nil {
			return "", fmt.Errorf("devicekeys: reading the passphrase file: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("devicekeys: reading the passphrase: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readTerminal asks for a secret on the controlling terminal, without echo. It
// uses the terminal device rather than stdin so a command whose stdin carries
// something else (the Personal MCP's JSON-RPC, a piped secret) can still ask.
func readTerminal(prompt string) (string, error) {
	in, out, done, err := openTerminal()
	if err != nil {
		return "", err
	}
	defer done()
	fmt.Fprint(out, prompt)
	b, err := term.ReadPassword(int(in.Fd())) // #nosec G115 -- a file descriptor, which fits an int
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("devicekeys: reading the passphrase: %w", err)
	}
	return string(b), nil
}

// openTerminal opens the controlling terminal, or falls back to stdin when it
// is one (a platform with no /dev/tty).
func openTerminal() (*os.File, io.Writer, func(), error) {
	if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		if term.IsTerminal(int(tty.Fd())) { // #nosec G115 -- a file descriptor, which fits an int
			return tty, tty, func() { _ = tty.Close() }, nil
		}
		_ = tty.Close()
	}
	if term.IsTerminal(int(os.Stdin.Fd())) { // #nosec G115 -- a file descriptor, which fits an int
		return os.Stdin, os.Stderr, func() {}, nil
	}
	return nil, nil, nil, fmt.Errorf("%w: run it from a terminal, or set %s to a file holding the passphrase",
		ErrNoTerminal, PassphraseFileEnvVar)
}
