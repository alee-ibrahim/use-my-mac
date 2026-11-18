package fleet

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// promptNoEcho reads one line from the terminal with echo disabled.
func promptNoEcho(label string) ([]byte, error) {
	fmt.Fprint(os.Stderr, label)
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, fmt.Errorf("cannot prompt for a secret: stdin is not a terminal")
	}
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return b, err
}
