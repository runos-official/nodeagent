package main

import (
	"fmt"
	"os"

	"github.com/runos-official/nodeagent/cmd"
	"github.com/runos-official/nodeagent/roslog"
)

func main() {
	if err := cmd.Execute(); err != nil {
		// roslog.Fail already printed the canonical failure block; only add the
		// generic line for errors that have not reported themselves.
		if !roslog.IsAlreadyReported(err) {
			fmt.Fprintln(os.Stderr, "There was an error executing the command:", err)
		}
		os.Exit(1)
	}
}
