package version

import (
	"fmt"

	"github.com/runos-official/nodeagent/version"
	"github.com/spf13/cobra"
)

var RootCmd = &cobra.Command{
	Use:   "version",
	Short: "Get the installed RunOS Node Agent version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(version.Version)
	},
}
