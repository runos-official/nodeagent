package etcd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/runos-official/nodeagent/k8s"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
)

var (
	nodeIP    string
	memberId  string
	assumeYes bool
)

var removeMember = &cobra.Command{
	Use:   "remove",
	Short: "Remove an etcd member",
	Long: `Remove an etcd member from the cluster, identified by exactly one of --ip or
--id.

The removal is quorum-guarded: it refuses to proceed if the cluster is
unhealthy or if removing the member would drop the cluster below the members
needed to keep quorum. This is destructive; you are prompted to confirm unless
--yes is given.`,
	Example: `  runos etcd remove --id 8e9e05c52164694d
  runos etcd remove --ip 10.0.0.7
  runos etcd remove --id 8e9e05c52164694d --yes`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Exactly one of --ip / --id, enforced by MarkFlagsMutuallyExclusive +
		// MarkFlagsOneRequired in init(). Build a human description for prompts
		// and messages.
		var target string
		if nodeIP != "" {
			target = fmt.Sprintf("IP %s", nodeIP)
		} else {
			target = fmt.Sprintf("ID %s", memberId)
		}

		if !assumeYes {
			confirmed, err := confirm(fmt.Sprintf(
				"About to remove etcd member with %s. This is destructive and cannot be undone. Continue? [y/N]: ", target))
			if err != nil {
				return roslog.Fail("Remove etcd member", err.Error(),
					"re-run with --yes to skip the interactive confirmation")
			}
			if !confirmed {
				roslog.Println("Aborted; no member was removed.")
				return nil
			}
		}

		// Route through the quorum-guarded direct path, which verifies health and
		// quorum before touching the cluster.
		var err error
		if nodeIP != "" {
			err = k8s.RemoveEtcdMemberViaEtcdCtlByIP(nodeIP)
		} else {
			err = k8s.RemoveEtcdMemberDirect(memberId)
		}
		if err != nil {
			return roslog.Fail("Remove etcd member", err.Error(),
				"verify the member with `runos etcd list`, then retry with the correct --id or --ip")
		}

		roslog.Printf("Successfully removed etcd member with %s\n", target)
		return nil
	},
}

// confirm prints prompt to stdout and reads a yes/no answer from stdin. Any
// answer other than y/yes (case-insensitive) is treated as no.
func confirm(prompt string) (bool, error) {
	roslog.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func init() {
	removeMember.Flags().StringVar(&nodeIP, "ip", "", "IP address of the etcd member to remove")
	removeMember.Flags().StringVar(&memberId, "id", "", "Member ID to remove (find it with `runos etcd list`)")
	removeMember.Flags().BoolVarP(&assumeYes, "yes", "y", false, "Skip the interactive confirmation prompt")

	removeMember.MarkFlagsMutuallyExclusive("ip", "id")
	removeMember.MarkFlagsOneRequired("ip", "id")
}
