package kubeproxy

import (
	"bufio"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var (
	configFile string
)

var listBackends = &cobra.Command{
	Use:   "list",
	Short: "List HAProxy backend servers",
	Run: func(cmd *cobra.Command, args []string) {
		backends, err := getHAProxyBackends(configFile)
		if err != nil {
			cmd.Println("Error listing HAProxy backends:", err)
			return
		}

		if len(backends) == 0 {
			cmd.Println("No HAProxy backends found")
			return
		}

		cmd.Println("HAProxy backends:")
		for backendName, servers := range backends {
			cmd.Printf("Backend: %s\n", backendName)
			for _, server := range servers {
				cmd.Printf("  Server: %s, Address: %s\n", server.name, server.address)
			}
		}
	},
}

type serverInfo struct {
	name    string
	address string
}

func getHAProxyBackends(configPath string) (map[string][]serverInfo, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	backends := make(map[string][]serverInfo)
	scanner := bufio.NewScanner(file)

	var currentBackend string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}

		// Check for backend definition
		if strings.HasPrefix(line, "backend ") {
			currentBackend = strings.TrimPrefix(line, "backend ")
			currentBackend = strings.TrimSpace(strings.Split(currentBackend, " ")[0])
			backends[currentBackend] = []serverInfo{}
			continue
		}

		// If in a backend section, look for server definitions
		if currentBackend != "" && strings.HasPrefix(line, "server ") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				serverName := parts[1]
				serverAddr := parts[2]

				backends[currentBackend] = append(backends[currentBackend], serverInfo{
					name:    serverName,
					address: serverAddr,
				})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return backends, nil
}

func init() {
	listBackends.Flags().StringVar(&configFile, "config", "/etc/haproxy/haproxy.cfg", "Path to HAProxy configuration file")
}
