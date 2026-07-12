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
// terminal, or plainly from piped stdin for scripts.
func readLineNoEcho() string {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr) // newline after the hidden input
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	line, _ := stdinReader.ReadString('\n')
	return strings.TrimSpace(line)
}

// promptLine prints a label and reads a visible line (for non-secret input).
func promptLine(label string) string {
	fmt.Fprint(os.Stderr, label)
	line, _ := stdinReader.ReadString('\n')
	return strings.TrimSpace(line)
}
