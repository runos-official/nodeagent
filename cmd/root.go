package cmd

import (
	"github.com/runos-official/nodeagent/cmd/agent"
	"github.com/runos-official/nodeagent/cmd/certificate"
	"github.com/runos-official/nodeagent/cmd/etcd"
	"github.com/runos-official/nodeagent/cmd/install"
	"github.com/runos-official/nodeagent/cmd/kubeproxy"
	"github.com/runos-official/nodeagent/cmd/logs"
	"github.com/runos-official/nodeagent/cmd/preflight"
	"github.com/runos-official/nodeagent/cmd/register"
	"github.com/runos-official/nodeagent/cmd/setconfig"
	"github.com/runos-official/nodeagent/cmd/status"
	"github.com/runos-official/nodeagent/cmd/sync"
	"github.com/runos-official/nodeagent/cmd/test"
	"github.com/runos-official/nodeagent/cmd/uninstall"
	"github.com/runos-official/nodeagent/cmd/update"
	"github.com/runos-official/nodeagent/cmd/version"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	// Used for flags.
	cfgFile string

	rootCmd = &cobra.Command{
		Use:   "runos",
		Short: "The RunOS Node Agent CLI",
	}
)

// Execute executes the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.AddCommand(version.RootCmd)
	rootCmd.AddCommand(preflight.RootCmd)
	rootCmd.AddCommand(register.RootCmd)
	rootCmd.AddCommand(install.RootCmd)
	rootCmd.AddCommand(agent.RootCmd)
	rootCmd.AddCommand(sync.RootCmd)
	rootCmd.AddCommand(test.RootCmd)
	rootCmd.AddCommand(update.RootCmd)
	rootCmd.AddCommand(uninstall.RootCmd)
	rootCmd.AddCommand(etcd.RootCmd)
	rootCmd.AddCommand(kubeproxy.RootCmd)
	rootCmd.AddCommand(setconfig.RootCmd)
	rootCmd.AddCommand(certificate.RootCmd)
	rootCmd.AddCommand(status.RootCmd)
	rootCmd.AddCommand(logs.RootCmd)
}

func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		// Use system config directory
		viper.AddConfigPath("/etc/runos")
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
	}

	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil {
		roslog.D("Using config file", "file", viper.ConfigFileUsed())
	}
}
