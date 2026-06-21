package etcd

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/runos-official/nodeagent/k8s"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
)

var listJSON bool

var listMembers = &cobra.Command{
	Use:   "list",
	Short: "List etcd members with detailed cluster information",
	Long: `List the etcd cluster overview (leader, raft index, DB size, health) and a
per-member table (ID, name, status, health, role, peer/client URLs).

Use --json for stable machine-readable output suitable for scripting.`,
	Example: `  runos etcd list
  runos etcd list --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		clusterInfo, err := k8s.GetEtcdClusterInfo()
		if err != nil {
			return roslog.Fail("List etcd members", err.Error(),
				"confirm this is a control-plane node and etcd is running (kubectl -n kube-system get pods), then retry")
		}

		if listJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(clusterInfo); err != nil {
				return roslog.Fail("List etcd members", err.Error(),
					"this is an internal encoding error; re-run without --json and report it")
			}
			return nil
		}

		if len(clusterInfo.Members) == 0 {
			roslog.Println("No etcd members found")
			return nil
		}

		printClusterInfo(clusterInfo)
		return nil
	},
}

// healthLabel renders a bool cluster-health flag as a human word instead of a
// raw Go bool.
func healthLabel(healthy bool) string {
	if healthy {
		return "healthy"
	}
	return "unhealthy"
}

// memberRole derives the role label for a member.
func memberRole(m k8s.EtcdMemberV2) string {
	switch {
	case m.IsLeader:
		return "Leader"
	case m.IsLearner:
		return "Learner"
	default:
		return "Follower"
	}
}

// memberStatus renders the started flag as a word.
func memberStatus(started bool) string {
	if started {
		return "started"
	}
	return "not started"
}

// printClusterInfo writes the human-readable cluster overview and member table
// to stdout.
func printClusterInfo(clusterInfo *k8s.EtcdClusterInfo) {
	roslog.Println("=== Etcd Cluster Information ===")
	roslog.Printf("Cluster ID:      %s\n", clusterInfo.ClusterID)
	roslog.Printf("Leader:          %s (%s)\n", clusterInfo.LeaderName, clusterInfo.LeaderID)
	// parts[8] of `etcdctl endpoint status` is the raft index, not the etcd
	// revision; label it accordingly.
	roslog.Printf("Raft Index:      %s\n", clusterInfo.RaftIndex)
	roslog.Printf("Database Size:   %.2f MB\n", float64(clusterInfo.DbSizeBytes)/1024/1024)
	roslog.Printf("Overall Health:  %s\n", healthLabel(clusterInfo.IsHealthy))
	roslog.Printf("Total Members:   %d\n\n", len(clusterInfo.Members))

	roslog.Println("=== Member Details ===")
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSTATUS\tHEALTH\tROLE\tPEER URLS\tCLIENT URLS")
	for _, member := range clusterInfo.Members {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			member.ID,
			member.Name,
			memberStatus(member.Started),
			member.Health,
			memberRole(member),
			member.PeerURLs,
			member.ClientURLs,
		)
	}
	w.Flush()
}

func init() {
	listMembers.Flags().BoolVar(&listJSON, "json", false, "Output cluster info as JSON")
}
