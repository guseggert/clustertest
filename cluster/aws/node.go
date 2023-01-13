package aws

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/guseggert/clustertest/agent"
	clusteriface "github.com/guseggert/clustertest/cluster"
)

type Node struct {
	agentClient *agent.Client
	sess        *session.Session
	ec2Client   *ec2.EC2
	accountID   string
	instanceID  string

	heartbeatOnce     sync.Once
	stopHeartbeatOnce sync.Once
	stopHeartbeat     chan struct{}
}

func (n *Node) StartProc(ctx context.Context, req clusteriface.StartProcRequest) (clusteriface.Process, error) {
	return n.agentClient.StartProc(ctx, req)
}

func (n *Node) SendFile(ctx context.Context, filePath string, contents io.Reader) error {
	return n.agentClient.SendFile(ctx, filePath, contents)
}

func (n *Node) ReadFile(ctx context.Context, path string) (io.ReadCloser, error) {
	return n.agentClient.ReadFile(ctx, path)
}

func (n *Node) Heartbeat(ctx context.Context) error {
	return n.agentClient.SendHeartbeat(ctx)
}

func (n *Node) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return n.agentClient.DialContext(ctx, network, addr)
}

func (n *Node) Fetch(ctx context.Context, url, path string) error {
	return n.agentClient.Fetch(ctx, url, path)
}

func (n *Node) StartHeartbeat() {
	go n.heartbeatOnce.Do(func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-n.stopHeartbeat:
				return
			case <-ticker.C:
			}
			err := n.Heartbeat(context.Background())
			if err != nil {
				log.Printf("heartbeat error: %s", err)
			}
		}
	})
}

func (n *Node) StopHeartbeat() {
	n.stopHeartbeatOnce.Do(func() { close(n.stopHeartbeat) })
}

func (n *Node) Stop(ctx context.Context) error {
	_, err := n.ec2Client.TerminateInstancesWithContext(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []*string{&n.instanceID},
	})
	if err != nil {
		return fmt.Errorf("stopping instance %q: %w", n.instanceID, err)
	}
	return nil
}

func (n *Node) String() string {
	return fmt.Sprintf("EC2 instance region=%s account=%s instanceID=%s", *n.sess.Config.Region, n.accountID, n.instanceID)
}
