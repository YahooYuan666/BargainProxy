// Command hashgen prints a bcrypt hash for the given password argument.
// Used only by local smoke scripts; not part of the gateway.
package main

import (
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hashgen <password>")
		os.Exit(1)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(os.Args[1]), bcrypt.MinCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(string(h))
}
