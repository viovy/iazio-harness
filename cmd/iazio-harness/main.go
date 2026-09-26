// Command iazio-harness runs one stored prompt and exits.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/viovy/iazio-harness/internal/auth"
)

var (
	version = "0.1.0-dev"
	commit  = "unknown"
	branch  = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: iazio-harness version|auth|run")
	}
	switch args[0] {
	case "version":
		fmt.Println(formatVersion())
		return nil
	case "auth":
		if len(args) > 1 && args[1] == "login" {
			return auth.Login(context.Background())
		}
		st, err := auth.Status()
		if err != nil {
			return err
		}
		fmt.Println(st.String())
		return nil
	case "run":
		return nil
	default:
		return fmt.Errorf("unknown command")
	}
}

func formatVersion() string {
	if commit == "" || commit == "unknown" {
		return "iazio-harness " + version
	}
	sha := commit
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return "iazio-harness " + version + "+" + sha
}
