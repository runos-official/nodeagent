package setconfig

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var RootCmd = &cobra.Command{
	Use:   "set-config <key> <value>",
	Short: "Set a configuration value",
	Long: `Set a configuration value and persist it to the config file.

Examples:
  runos set-config client.server.installer https://my-server.com
  runos set-config client.server.nodeward nodeward.runos.com
  runos set-config node.ip 192.168.1.100`,
	Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		key := args[0]
		value := args[1]

		// Set the value in viper
		viper.Set(key, value)

		// Write the config file
		if err := viper.WriteConfig(); err != nil {
			fmt.Printf("Error writing config file: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Successfully set %s = %s\n", key, value)
		fmt.Printf("Config file updated: %s\n", viper.ConfigFileUsed())
	},
}
