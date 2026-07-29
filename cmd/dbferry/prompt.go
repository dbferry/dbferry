package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// stdinReader is a single shared buffered reader over stdin. Creating a new
// bufio.Reader per prompt would over-read piped input into a discarded buffer
// and lose subsequent lines.
var stdinReader = bufio.NewReader(os.Stdin)

// readLineNoEcho reads one line of secret input: hidden (no echo) from a
// terminal, or plainly from piped stdin for scripts. Only the line ending is
// stripped — a password with leading or trailing spaces must arrive intact,
// or auth fails later with no hint of why.
func readLineNoEcho() string {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr) // newline after the hidden input
		if err != nil {
			return ""
		}
		return strings.TrimRight(string(b), "\r\n")
	}
	line, _ := stdinReader.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// promptLine prints a label and reads a visible line (for non-secret input).
func promptLine(label string) string {
	fmt.Fprint(os.Stderr, label)
	line, _ := stdinReader.ReadString('\n')
	return strings.TrimSpace(line)
}

// confirmTTY asks a yes/no question on an interactive terminal. The second
// return value is false when stdin is not a terminal — callers then require an
// explicit --yes rather than assuming consent, so scripts never delete by
// accident.
func confirmTTY(label string) (yes, interactive bool) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, false
	}
	fmt.Fprint(os.Stderr, label+" [y/N]: ")
	line, _ := stdinReader.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", true
}
