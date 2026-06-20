package config

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/viper"
)

func init() {
	// Set the base name of the config file, without the file extension.
	viper.SetConfigName("config")

	// Set the type of the config file.
	viper.SetConfigType("yaml")

	configPath := "/etc/runos"
	roslog.D("RunOS Node Agent config path configured", "path", configPath)

	viper.AddConfigPath(configPath)

	// Assuming viper is already set up and the config path is set
	roslog.D("Using config path", "path", configPath)

	// Check if the config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		roslog.D("Config directory does not exist", "path", configPath)

		// Try to create the directory if it doesn't exist
		// But don't panic if we can't (e.g., for commands that don't need config)
		if err := os.MkdirAll(configPath, 0755); err != nil {
			roslog.D("Could not create config directory (this may be OK for some commands)", "error", err.Error())
		}
	}

	viper.SetDefault("client.aid", "xxxxx")
	viper.SetDefault("client.server.installer", "https://get.runos.com")
	viper.SetDefault("client.server.nodeward", "nodeward.runos.com")
	viper.SetDefault("node.nid", "xxxxx")
	viper.SetDefault("node.ip", assumeDefaultNodeIp())
	viper.SetDefault("mtls.crt", configPath+"/mtls.crt")
	viper.SetDefault("mtls.key", configPath+"/mtls.key")
	viper.SetDefault("mtls.ca", configPath+"/ca.crt")
	viper.SetDefault("mtls.public-ca", configPath+"/l1sec-ca.runos.public.pem")
	// Attempt to read the config file.
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// The config file was not found; try to create it with default values.
			roslog.D("No config file found. Attempting to create with default values.")
			if err := viper.SafeWriteConfig(); err != nil {
				// Don't panic - some commands (like preflight, help) don't need config
				roslog.D("Could not create config file (this may be OK for some commands)", "error", err.Error())
			}
		} else {
			// Found but another error was produced
			roslog.D("Error reading config file (this may be OK for some commands)", "error", err.Error())
		}
	}
}

func assumeDefaultNodeIp() string {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	for _, addr := range addresses {
		// Check if the address is not a loopback address and is an IPv4 address
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
			return ipNet.IP.String()
		}
	}

	panic("No suitable IP address found")
}

// GetAID returns the configured account ID.
func GetAID() string {
	return viper.GetString("client.aid")
}

// GetNodeIP returns the node's external IP, auto-detecting one if unset.
func GetNodeIP() string {
	ret := viper.GetString("node.ip")
	if ret == "" {
		viper.Set("node.ip", assumeDefaultNodeIp())
	}

	return viper.GetString("node.ip")
}

// GetNID returns the configured node ID.
func GetNID() string {
	return viper.GetString("node.nid")
}

// GetUID returns the configured user ID.
func GetUID() string {
	return viper.GetString("client.uid")
}

// GetKey returns the configured client key.
func GetKey() string {
	return viper.GetString("client.key")
}

// GetServer returns the configured server.
func GetServer() string {
	return viper.GetString("client.server")
}

// UpdateConfig persists the given account ID and node ID to the config file.
func UpdateConfig(aid string, nid string) {
	viper.Set("client.aid", aid)
	viper.Set("node.nid", nid)
	if err := viper.WriteConfig(); err != nil {
		roslog.E("Error writing config file", err)
		panic(err)
	}
}

// SetNodewardHost persists the nodeward server host in config. (Formerly
// UpdateTargetServerFromInstallUrl, which parsed a URL; it now just stores the
// host that the caller already resolved.)
func SetNodewardHost(nodewardServerHost string) {
	roslog.I("Setting nodeward host", "nodewardServerHost", nodewardServerHost)
	viper.Set("client.server.nodeward", nodewardServerHost)
}

// GetPublicKeyPath returns the path to the mTLS client certificate, creating an
// empty file at that path if it does not yet exist.
func GetPublicKeyPath() string {
	caPath := viper.GetString("mtls.crt")
	createFileIfNotExists(caPath, 0644) // public certificate
	return caPath
}

// GetPrivateKeyPath returns the path to the mTLS private key, creating an empty
// file at that path if it does not yet exist.
func GetPrivateKeyPath() string {
	caPath := viper.GetString("mtls.key")
	createFileIfNotExists(caPath, 0600) // private key: root-only
	return caPath
}

// GetCACertPath returns the path to the mTLS CA certificate, creating an empty
// file at that path if it does not yet exist.
func GetCACertPath() string {
	caPath := viper.GetString("mtls.ca")
	createFileIfNotExists(caPath, 0644) // public CA certificate
	return caPath
}

// GetPublicCACertPath returns the path to the public CA certificate used for the
// L1Sec registration channel.
func GetPublicCACertPath() string {
	caPath := viper.GetString("mtls.public-ca")
	return caPath
}

//func GetConfigValueByPath(path string) string {
//	return viper.GetString(path)
//}

// GetROSInstallerURL returns the configured RunOS installer base URL.
func GetROSInstallerURL() string {
	return viper.GetString("client.server.installer")
}

// GetConductorURL returns the configured Conductor base URL, deriving it from
// the installer URL (and persisting it) if not explicitly set.
func GetConductorURL() string {
	conductor := viper.GetString("client.server.conductor")
	if conductor != "" {
		return conductor
	}

	installer := GetROSInstallerURL()
	conductor = strings.Replace(installer, "get.", "api.", 1)
	viper.Set("client.server.conductor", conductor)
	if err := viper.WriteConfig(); err != nil {
		roslog.E("Error writing config file", err)
	}
	return conductor
}

// GetNodewardHost returns the configured Nodeward control plane host.
func GetNodewardHost() string {
	return viper.GetString("client.server.nodeward")
}

// GetCPDomain returns the configured control plane domain, if any.
func GetCPDomain() string {
	return viper.GetString("client.server.cp.domain")
}

// createFileIfNotExists creates an empty file at filename with the given perm if
// it does not already exist. Use 0600 for secrets (the mTLS private key) and 0644
// for public material (certs, CA). It does not relax the mode of an existing file.
func createFileIfNotExists(filename string, perm os.FileMode) {
	_, err := os.Stat(filename)
	if os.IsNotExist(err) {
		file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			roslog.E("Failed creating file", err)
			panic(err)
		}
		file.Close()
	} else if err != nil {
		roslog.E("Failed checking if file exists", err)
		panic(err)
	}
}
