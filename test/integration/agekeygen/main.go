// Command agekeygen generates a throwaway age identity for the integration
// stand (poc-plan 0.2). It mirrors what a real customer does with `age-keygen`
// under BYOK (see DECISIONS.md), so integration tests can encrypt to the
// recipient and decrypt on restore without a real key ever entering the repo.
//
// It writes two files into an output directory:
//
//	age-identity.txt   the secret key (AGE-SECRET-KEY-1...), mode 0600
//	age-recipient.txt  the public recipient (age1...)
//
// The output directory is gitignored: the key is disposable and local-only.
// If age-identity.txt already exists, the tool is a no-op so the stand keeps a
// stable recipient across restarts.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"filippo.io/age"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agekeygen: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	outDir := flag.String("out", ".stand", "directory to write the identity and recipient into")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		return err
	}

	idPath := filepath.Join(*outDir, "age-identity.txt")
	recPath := filepath.Join(*outDir, "age-recipient.txt")

	if _, err := os.Stat(idPath); err == nil {
		// Identity already present: keep the existing recipient stable.
		rec, readErr := os.ReadFile(recPath)
		if readErr != nil {
			return fmt.Errorf("identity exists but recipient unreadable: %w", readErr)
		}
		fmt.Printf("age identity already present at %s\nrecipient: %s", idPath, rec)
		return nil
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		return err
	}

	if err := os.WriteFile(idPath, []byte(id.String()+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(recPath, []byte(id.Recipient().String()+"\n"), 0o644); err != nil {
		return err
	}

	fmt.Printf("wrote %s (0600) and %s\nrecipient: %s\n", idPath, recPath, id.Recipient())
	return nil
}
