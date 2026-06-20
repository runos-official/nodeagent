package registernode

import (
	"context"
	"fmt"
	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/config"
	pb "github.com/runos-official/nodeagent/l1sec"
	"github.com/runos-official/nodeagent/roslog"
	"log"
	"time"
)

// RegisterNode registers this node with Nodeward over L1Sec, storing the issued
// mTLS certificates and CA, and persisting the resolved account and node IDs.
func RegisterNode(token, aid, machineId, cp, server string) {
	config.SetNodewardHost(server)

	c, _, backendCancel, conn := backend.NodewardL1Sec()
	defer backendCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer conn.Close()

	request := &pb.NodeRegistrationRequest{
		Token:     token,
		Aid:       aid,
		MachineId: machineId,
		Os:        commons.GetOSInfo(),
	}
	r, err := c.Register(ctx, request)
	if err != nil {
		log.Fatalf("Error executing Init2FA: %v", err)
	}

	fmt.Println("Writing CA certificate to file...")
	if err := StorePublicKey(r.CaCert, config.GetCACertPath()); err != nil {
		log.Fatalf("Error storing CA certificate: %v", err)
	}

	fmt.Println("Writing public certificate to file...")
	if err := StorePublicKey(r.PublicKey, config.GetPublicKeyPath()); err != nil {
		log.Fatalf("Error storing public certificate: %v", err)
	}

	fmt.Println("Writing private certificate to file...")
	if err := StorePrivateKey(r.PrivateKey, config.GetPrivateKeyPath()); err != nil {
		log.Fatalf("Error storing private certificate: %v", err)
	}

	config.UpdateConfig(aid, r.Nid)

	if err := backend.AddNodelog(3, "NodeInstallation", "Node agent has been registered."); err != nil {
		roslog.I("Could not add nodelog", err)
	}
}
