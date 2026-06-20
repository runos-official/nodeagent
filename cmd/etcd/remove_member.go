package etcd

import (
	"fmt"
	"github.com/runos-official/nodeagent/k8s"
	"log"

	"github.com/spf13/cobra"
)

var (
	nodeIP   string
	memberId string
)

var removeMember = &cobra.Command{
	Use:   "remove",
	Short: "Remove an etcd member",
	Run: func(cmd *cobra.Command, args []string) {
		if nodeIP != "" {
			// Remove the etcd member by IP
			if err := k8s.RemoveEtcdMemberByIP(nodeIP); err != nil {
				log.Fatalf("Error removing etcd member by ip: %v", err)
			}

			fmt.Printf("Successfully removed etcd member with IP %s\n", nodeIP)
		}
		if memberId != "" {
			// Remove the etcd member by IP
			if err := k8s.RemoveEtcdMemberByID(memberId); err != nil {
				log.Fatalf("Error removing etcd member by id: %v", err)
			}

			fmt.Printf("Successfully removed etcd member with id %s\n", memberId)
		} else {
			cmd.Help()
		}
	},
}

func init() {
	removeMember.Flags().StringVar(&nodeIP, "ip", "", "IP address of the etcd member to remove")
	removeMember.Flags().StringVar(&memberId, "id", "", "You can find the member id with etcd list")
}
