package kubeproxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
)

var (
	configFile string
	listJSON   bool
)

// Backend is the stable, machine-readable shape of a single HAProxy backend
// emitted by `runos kubeproxy list --json`.
type Backend struct {
	Name    string   `json:"name"`
	Servers []Server `json:"servers"`
}

// Server is a single server line inside an HAProxy backend.
type Server struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

var listBackends = &cobra.Command{
	Use:   "list",
	Short: "List HAProxy backend servers",
	Long: `List the backends defined in the local HAProxy configuration and the servers
in each, parsed from the HAProxy config file (default /etc/haproxy/haproxy.cfg).

Use --json for stable machine-readable output suitable for scripting.`,
	Example: `  runos kubeproxy list
  runos kubeproxy list --json
  runos kubeproxy list --config /etc/haproxy/haproxy.cfg`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		backends, err := getHAProxyBackends(configFile)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return roslog.Fail("List HAProxy backends",
					"HAProxy config not found at "+configFile,
					"run `runos kubeproxy refresh` to generate it, or pass --config with the correct path")
			}
			return roslog.Fail("List HAProxy backends", err.Error(),
				"confirm the HAProxy config path is readable, then retry")
		}

		// Sort backend names for deterministic output.
		names := make([]string, 0, len(backends))
		for name := range backends {
			names = append(names, name)
		}
		sort.Strings(names)

		if listJSON {
			out := make([]Backend, 0, len(names))
			for _, name := range names {
				servers := make([]Server, 0, len(backends[name]))
				for _, s := range backends[name] {
					servers = append(servers, Server{Name: s.name, Address: s.address})
				}
				out = append(out, Backend{Name: name, Servers: servers})
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if err := enc.Encode(out); err != nil {
				return roslog.Fail("List HAProxy backends", err.Error(),
					"this is an internal encoding error; re-run without --json and report it")
			}
			return nil
		}

		if len(names) == 0 {
			// Diagnostic note belongs on stderr, not stdout.
			cmd.PrintErrln("No HAProxy backends found")
			return nil
		}

		cmd.Println("HAProxy backends:")
		for _, name := range names {
			cmd.Printf("Backend: %s\n", name)
			for _, server := range backends[name] {
				cmd.Printf("  Server: %s, Address: %s\n", server.name, server.address)
			}
		}
		return nil
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
	listBackends.Flags().BoolVar(&listJSON, "json", false, "Output backends as JSON")
}
