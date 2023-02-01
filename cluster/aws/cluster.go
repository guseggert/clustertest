package aws

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/guseggert/clustertest/agent"
	clusteriface "github.com/guseggert/clustertest/cluster"
	"go.uber.org/zap"
)

const userDataTemplate = `#!/bin/bash
mkdir /node
cd /node
curl --retry 3 '{{.NodeAgentURL}}' > nodeagent
chmod +x nodeagent
nohup ./nodeagent \
  --heartbeat-timeout 1m \
  --on-heartbeat-failure shutdown \
  --ca-cert-pem {{.CACertPEMEncoded}} \
  --cert-pem {{.CertPEMEncoded}} \
  --key-pem {{.KeyPEMEncoded}} \
  &>/var/log/nodeagent &
`

type Cluster struct {
	Nodes []*Node

	InstanceType       string
	CleanupWait        bool
	RunInstancesConfig func(*ec2.RunInstancesInput) error

	ctx    context.Context
	config *config
}

func collectPages[IN any, OUT any](input IN, fn func(IN, func(OUT, bool) bool) error) ([]OUT, error) {
	var out []OUT
	err := fn(input, func(output OUT, more bool) bool {
		out = append(out, output)
		return true
	})
	return out, err
}

func collectPagesWithContext[IN any, OUT any](ctx context.Context, input IN, fn func(context.Context, IN, func(OUT, bool) bool, ...request.Option) error, opts ...request.Option) ([]OUT, error) {
	var out []OUT
	err := fn(ctx, input, func(output OUT, more bool) bool {
		out = append(out, output)
		return true
	}, opts...)
	return out, err
}

type Option func(c *Cluster)

// WithRunInstancesInput registers a callback for customizing RunInstances calls when new nodes are created.
func (c *Cluster) WithRunInstancesInput(f func(input *ec2.RunInstancesInput) error) *Cluster {
	c.RunInstancesConfig = f
	return c
}

func (c *Cluster) WithLogger(l *zap.SugaredLogger) *Cluster {
	c.config.log = l.Named("ec2_cluster")
	return c
}

func (c *Cluster) WithInstanceType(s string) *Cluster {
	c.InstanceType = s
	return c
}

func (c *Cluster) WithSession(sess *session.Session) *Cluster {
	c.config.session = sess
	return c
}

func (c *Cluster) WithNodeAgentBin(binPath string) *Cluster {
	c.config.nodeAgentBin = binPath
	return c
}

// WithCleanupWait causes the Cleanup methods to wait for instance termination to succeed before returning.
func (c *Cluster) WithCleanupWait() *Cluster {
	c.CleanupWait = true
	return c
}

func (c *Cluster) Context(ctx context.Context) *Cluster {
	newC := *c
	newC.ctx = ctx
	return &newC
}

// NewCluster creates a new AWS cluster.
// This uses standard AWS profile env vars.
// With no configuration, this uses the default profile.
// The user/role used must have the appropriate permissions for the test runner,
// in order to find the resources in the account and launch/destroy EC2 instances.
//
// By default, this looks for the node agent binary by searching up from PWD for a "nodeagent" file.
func NewCluster() *Cluster {
	return &Cluster{
		InstanceType: "t3.micro",
		ctx:          context.Background(),
		config:       &config{},
	}
}

func (c *Cluster) waitForInstances(ctx context.Context, instances []*ec2.Instance) ([]*ec2.Instance, error) {
	var instanceIDs []*string
	for _, inst := range instances {
		instanceIDs = append(instanceIDs, inst.InstanceId)
	}

	var newInstances []*ec2.Instance
	for i := 0; ; i++ {
		if i != 0 {
			time.Sleep(1 * time.Second)
		}
		out, err := c.config.ec2Client.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: instanceIDs,
		})
		if err != nil {
			// this can happen due to EC2 eventual consistency, ignore it and keep polling
			if awsErr, ok := err.(awserr.Error); ok {
				if awsErr.Code() == "InvalidInstanceID.NotFound" {
					continue
				}
			}
			return nil, fmt.Errorf("waiting for EC2 instance: %w", err)
		}
		newInstances = nil
		for _, res := range out.Reservations {
			for _, inst := range res.Instances {
				stateName := *inst.State.Name
				switch stateName {
				case ec2.InstanceStateNamePending:
					continue
				case ec2.InstanceStateNameRunning:
					newInstances = append(newInstances, inst)
				default:
					return nil, fmt.Errorf("unexpected instance state %q", stateName)
				}
			}
		}
		if len(newInstances) == len(instances) {
			return newInstances, nil
		}
	}
}

func (c *Cluster) NewNodes(ctx context.Context, n int) (clusteriface.Nodes, error) {
	if err := c.ensureLoaded(); err != nil {
		return nil, err
	}
	req, _ := c.config.s3Client.GetObjectRequest(&s3.GetObjectInput{
		Bucket: &c.config.nodeAgentS3Bucket,
		Key:    &c.config.nodeAgentS3Key,
	})
	nodeagentURL, err := req.Presign(5 * time.Minute)
	if err != nil {
		return nil, fmt.Errorf("presigning node agent URL: %w", err)
	}

	tmpl, err := template.New("").Parse(userDataTemplate)
	if err != nil {
		return nil, fmt.Errorf("parsing user data template: %w", err)
	}

	caCertPEMEncoded := base64.StdEncoding.EncodeToString(c.config.cert.CA.CertPEMBytes)
	certPEMEncoded := base64.StdEncoding.EncodeToString(c.config.cert.Server.CertPEMBytes)
	keyPEMEncoded := base64.StdEncoding.EncodeToString(c.config.cert.Server.KeyPEMBytes)

	buf := &bytes.Buffer{}
	err = tmpl.Execute(buf, map[string]string{
		"NodeAgentURL":     nodeagentURL,
		"CACertPEMEncoded": caCertPEMEncoded,
		"CertPEMEncoded":   certPEMEncoded,
		"KeyPEMEncoded":    keyPEMEncoded,
	})
	if err != nil {
		return nil, fmt.Errorf("executing user data template: %w", err)
	}

	userData := base64.StdEncoding.EncodeToString(buf.Bytes())

	n64 := int64(n)
	input := &ec2.RunInstancesInput{
		ImageId:                           &c.config.amiID,
		IamInstanceProfile:                &ec2.IamInstanceProfileSpecification{Arn: &c.config.instanceProfileARN},
		InstanceType:                      &c.InstanceType,
		MaxCount:                          &n64,
		MinCount:                          &n64,
		InstanceInitiatedShutdownBehavior: aws.String(ec2.ShutdownBehaviorTerminate),
		UserData:                          &userData,
		NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{{
			AssociatePublicIpAddress: aws.Bool(true),
			DeleteOnTermination:      aws.Bool(true),
			Groups:                   []*string{&c.config.instanceSecurityGroupID},
			SubnetId:                 &c.config.subnetID,
			DeviceIndex:              aws.Int64(0),
		}},
	}
	if c.RunInstancesConfig != nil {
		c.RunInstancesConfig(input)
	}

	reservations, err := c.config.ec2Client.RunInstancesWithContext(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("launching instance: %w", err)
	}

	if len(reservations.Instances) != n {
		return nil, fmt.Errorf("expected %d instances instance but got %d", n, len(reservations.Instances))
	}

	instances, err := c.waitForInstances(ctx, reservations.Instances)
	if err != nil {
		return nil, fmt.Errorf("waiting for instancs: %w", err)
	}

	var ifaceNodes clusteriface.Nodes
	var nodes []*Node
	for _, inst := range instances {
		nodeAgentClient, err := agent.NewClient(c.config.log, c.config.cert, *inst.PublicIpAddress, 8080)
		if err != nil {
			return nil, fmt.Errorf("constructing node agent client: %w", err)
		}
		node := &Node{
			agentClient: nodeAgentClient,
			sess:        c.config.session,
			ec2Client:   c.config.ec2Client,
			instanceID:  *inst.InstanceId,
			accountID:   c.config.accountID,
			cleanupWait: c.CleanupWait,
		}
		nodes = append(nodes, node)
		ifaceNodes = append(ifaceNodes, node)
		c.Nodes = append(c.Nodes, node)
		node.agentClient.StartHeartbeat()
	}

	err = c.waitForNodesHeartbeats(ctx, nodes)
	if err != nil {
		return nil, err
	}

	return ifaceNodes, nil
}

func (c *Cluster) waitForNodeHeartbeat(ctx context.Context, node *Node) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := node.Heartbeat(ctx)
			if err == nil {
				return
			}
		}
	}
}

func (c *Cluster) waitForNodesHeartbeats(ctx context.Context, nodes []*Node) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	wg := sync.WaitGroup{}
	wg.Add(len(nodes))
	// wait on a heartbeat
	for _, node := range nodes {
		node := node
		go func() {
			defer wg.Done()
			c.waitForNodeHeartbeat(ctx, node)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func (c *Cluster) NewNode(ctx context.Context) (clusteriface.Node, error) {
	if err := c.ensureLoaded(); err != nil {
		return nil, err
	}
	nodes, err := c.NewNodes(ctx, 1)
	if err != nil {
		return nil, err
	}
	return nodes[0], nil
}

func (c *Cluster) Cleanup(ctx context.Context) error {
	if err := c.ensureLoaded(); err != nil {
		return err
	}
	for _, n := range c.Nodes {
		err := n.Stop(ctx)
		if err != nil {
			return fmt.Errorf("stopping node %s: %w", n, err)
		}
	}
	return nil
}
