package setconfig

import (
	"fmt"
	"strings"

	// Imported for its side effect: config.init() seeds viper defaults and, on a
	// fresh node with no /etc/runos/config.yaml, bootstraps the file via
	// SafeWriteConfig. Without this, viper.WriteConfig() below would error on a
	// fresh node because no config file has ever been written.
	_ "github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// defaultConfigPath is where the node agent persists its config on a fresh node
// when no config file has been loaded yet (viper.ConfigFileUsed() == "").
const defaultConfigPath = "/etc/runos/config.yaml"

// protectedNamespaces are keys managed by `runos register` / the agent itself.
// Setting them by hand can break mTLS or node identity, so we warn (on stderr)
// but still allow it for advanced/recovery use.
var protectedNamespaces = []string{"mtls.", "client.aid", "node.nid"}

var RootCmd = &cobra.Command{
	Use:   "set-config <key> <value>",
	Short: "Set a configuration value",
	Long: `Set a configuration value and persist it to /etc/runos/config.yaml.

Keys are dotted paths into the YAML config. On a fresh node the config file is
created automatically; writing it requires permission to /etc/runos (run as root,
or pass --config <path> to target a writable file).`,
	Example: `  runos set-config client.server.installer https://my-server.com
  runos set-config client.server.nodeward nodeward.runos.com
  runos set-config node.ip 192.168.1.100`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := strings.TrimSpace(args[0])
		value := strings.TrimSpace(args[1])

		if key == "" {
			return roslog.Fail(
				"Set config",
				"the config key is empty",
				"pass a non-empty dotted key, e.g. runos set-config node.ip 192.168.1.100",
			)
		}
		if value == "" {
			return roslog.Fail(
				"Set config",
				fmt.Sprintf("no value given for %q", key),
				"pass a non-empty value, e.g. runos set-config "+key+" <value>",
			)
		}

		// Warn (on stderr, so stdout stays clean for piping) when overwriting a
		// key normally managed by `runos register` or the agent itself.
		for _, ns := range protectedNamespaces {
			if key == ns || strings.HasPrefix(key, ns) {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"%sWarning:%s %q is normally managed by RunOS; setting it by hand may break mTLS or node identity.\n",
					roslog.ColorYellow, roslog.ColorReset, key)
				break
			}
		}

		// Set the value in viper.
		viper.Set(key, value)

		// Persist. On a fresh node viper has no config file loaded yet
		// (ConfigFileUsed() == ""), so WriteConfig would fail; write to the
		// default path instead. config.init() has already created/seeded
		// /etc/runos/config.yaml when possible, but cover the case where it
		// could not (e.g. directory just became writable).
		var err error
		if viper.ConfigFileUsed() == "" {
			err = viper.WriteConfigAs(defaultConfigPath)
		} else {
			err = viper.WriteConfig()
		}
		if err != nil {
			return roslog.Fail(
				"Set config",
				err.Error(),
				"run as root so /etc/runos/config.yaml is writable, or pass --config <path>",
			)
		}

		path := viper.ConfigFileUsed()
		if path == "" {
			path = defaultConfigPath
		}
		roslog.Printf("Successfully set %s = %s\n", key, value)
		roslog.Printf("Config file updated: %s\n", path)
		return nil
	},
}
