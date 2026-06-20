package etcd

import (
	"github.com/runos-official/nodeagent/k8s"
	"github.com/spf13/cobra"
)

var listMembers = &cobra.Command{
	Use:   "list",
	Short: "List the etcd members with detailed cluster information",
	Run: func(cmd *cobra.Command, args []string) {
		clusterInfo, err := k8s.GetEtcdClusterInfo()
		if err != nil {
			cmd.Println("Error getting etcd cluster info:", err)
			return
		}

		if len(clusterInfo.Members) == 0 {
			cmd.Println("No etcd members found")
			return
		}

		// Print cluster overview
		cmd.Println("=== Etcd Cluster Information ===")
		cmd.Printf("Cluster ID: %s\n", clusterInfo.ClusterID)
		cmd.Printf("Leader: %s (%s)\n", clusterInfo.LeaderName, clusterInfo.LeaderID)
		cmd.Printf("Revision: %s\n", clusterInfo.Revision)
		cmd.Printf("Database Size: %.2f MB\n", float64(clusterInfo.DbSizeBytes)/1024/1024)
		cmd.Printf("Overall Health: %v\n", clusterInfo.IsHealthy)
		cmd.Printf("Total Members: %d\n\n", len(clusterInfo.Members))

		// Print member details
		cmd.Println("=== Member Details ===")
		for i, member := range clusterInfo.Members {
			cmd.Printf("Member %d:\n", i+1)
			cmd.Printf("  ID: %s\n", member.ID)
			cmd.Printf("  Name: %s\n", member.Name)
			cmd.Printf("  Status: %s\n", func() string {
				if member.Started {
					return "started"
				}
				return "not started"
			}())
			cmd.Printf("  Health: %s\n", member.Health)
			cmd.Printf("  Role: %s\n", func() string {
				if member.IsLeader {
					return "Leader"
				} else if member.IsLearner {
					return "Learner"
				}
				return "Follower"
			}())
			cmd.Printf("  Peer URLs: %s\n", member.PeerURLs)
			cmd.Printf("  Client URLs: %s\n", member.ClientURLs)
			if i < len(clusterInfo.Members)-1 {
				cmd.Println()
			}
		}
	},
}
