package aws

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/guseggert/clustertest/agent"
	clusteriface "github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/internal/files"
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
	InstanceProfileARN      string
	InstanceSecurityGroupID string
	InstanceType            string
	AMIID                   string
	AccountID               string
	SubnetID                string
	Session                 *session.Session
	NodeAgentBin            string
	NodeAgentS3Bucket       string
	NodeAgentS3Key          string
	EC2Client               *ec2.EC2
	S3Client                *s3.S3
	RunInstancesConfig      func(*ec2.RunInstancesInput) error
	Cert                    *agent.Certs
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

func fetchAMIID(sess *session.Session) (string, error) {
	ssmClient := ssm.New(sess)
	ssmKey := "/aws/service/ami-amazon-linux-latest/amzn2-ami-hvm-x86_64-gp2"
	res, err := ssmClient.GetParameters(&ssm.GetParametersInput{Names: []*string{&ssmKey}})
	if err != nil {
		return "", fmt.Errorf("fetching AMI ID: %w", err)
	}
	return *res.Parameters[0].Value, nil
}

type Option func(c *Cluster)

// WithRunInstancesInput registers a callback for customizing RunInstances calls when new nodes are created.
func WithRunInstancesInput(f func(input *ec2.RunInstancesInput) error) Option {
	return func(c *Cluster) {
		c.RunInstancesConfig = f
	}
}

// provideFileViaS3 uploads the file at the path to S3 with a random key, and returns the key.
func provideFileViaS3(sess *session.Session, bucket, path string) (string, error) {
	s3Client := s3.New(sess)

	// use the hash of the file as the key for deduping
	hasher := sha256.New()
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening to compute S3 key: %w", err)
	}
	_, err = io.Copy(hasher, f)
	if err != nil {
		return "", fmt.Errorf("hashing file to write to S3: %w", err)
	}
	key := base32.StdEncoding.EncodeToString(hasher.Sum(nil))

	f, err = os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening to send to S3: %w", err)
	}
	_, err = s3Client.PutObject(&s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   f,
	})
	if err != nil {
		return "", fmt.Errorf("putting to S3: %w", err)
	}
	return key, nil
}

func NewCluster(opts ...Option) (*Cluster, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getting wd: %w", err)
	}
	nodeAgentBin := files.FindUp("nodeagent", wd)
	if nodeAgentBin == "" {
		return nil, errors.New("unable to find nodeagent bin")
	}

	sess, err := session.NewSessionWithOptions(session.Options{SharedConfigState: session.SharedConfigEnable})
	if err != nil {
		return nil, fmt.Errorf("creating AWS Go SDK session: %w", err)
	}

	outputsMap, err := fetchStackOutputs(sess)
	if err != nil {
		return nil, fmt.Errorf("fetching stack outputs: %w", err)
	}

	outputs, err := parseStackOutputs(outputsMap)
	if err != nil {
		return nil, fmt.Errorf("parsing stack outputs: %w", err)
	}

	// upload the node agent to S3
	nodeFilesKey, err := provideFileViaS3(sess, outputs.s3Bucket, nodeAgentBin)
	if err != nil {
		return nil, fmt.Errorf("uploading node agent to S3: %w", err)
	}

	// TODO: allow pinning the AMI ID
	amiID, err := fetchAMIID(sess)
	if err != nil {
		return nil, fmt.Errorf("fetching AMI ID: %w", err)
	}

	cert, err := agent.GenerateCert()
	if err != nil {
		return nil, fmt.Errorf("generating cert: %w", err)
	}

	c := &Cluster{
		InstanceProfileARN:      outputs.ec2InstanceProfileARN,
		InstanceSecurityGroupID: outputs.ec2SecurityGroupID,
		AccountID:               outputs.accountID,
		SubnetID:                outputs.publicSubnetIDs[0],
		AMIID:                   amiID,
		Session:                 sess,
		EC2Client:               ec2.New(sess),
		S3Client:                s3.New(sess),
		InstanceType:            "t3.nano",
		NodeAgentBin:            nodeAgentBin,
		NodeAgentS3Key:          nodeFilesKey,
		NodeAgentS3Bucket:       outputs.s3Bucket,
		Cert:                    cert,
	}

	for _, o := range opts {
		o(c)
	}

	return c, nil
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
		out, err := c.EC2Client.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{
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
	req, _ := c.S3Client.GetObjectRequest(&s3.GetObjectInput{
		Bucket: &c.NodeAgentS3Bucket,
		Key:    &c.NodeAgentS3Key,
	})
	nodeagentURL, err := req.Presign(5 * time.Minute)
	if err != nil {
		return nil, fmt.Errorf("presigning node agent URL: %w", err)
	}

	tmpl, err := template.New("").Parse(userDataTemplate)
	if err != nil {
		return nil, fmt.Errorf("parsing user data template: %w", err)
	}

	caCertPEMEncoded := base64.StdEncoding.EncodeToString(c.Cert.CA.CertPEMBytes)
	certPEMEncoded := base64.StdEncoding.EncodeToString(c.Cert.Server.CertPEMBytes)
	keyPEMEncoded := base64.StdEncoding.EncodeToString(c.Cert.Server.KeyPEMBytes)

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
		ImageId:                           &c.AMIID,
		IamInstanceProfile:                &ec2.IamInstanceProfileSpecification{Arn: &c.InstanceProfileARN},
		InstanceType:                      &c.InstanceType,
		MaxCount:                          &n64,
		MinCount:                          &n64,
		InstanceInitiatedShutdownBehavior: aws.String(ec2.ShutdownBehaviorTerminate),
		UserData:                          &userData,
		NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{{
			AssociatePublicIpAddress: aws.Bool(true),
			DeleteOnTermination:      aws.Bool(true),
			Groups:                   []*string{&c.InstanceSecurityGroupID},
			SubnetId:                 &c.SubnetID,
			DeviceIndex:              aws.Int64(0),
		}},
	}
	if c.RunInstancesConfig != nil {
		c.RunInstancesConfig(input)
	}

	reservations, err := c.EC2Client.RunInstancesWithContext(ctx, input)
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
	var nodes []*node
	for _, inst := range instances {
		nodeAgentClient, err := agent.NewClient(c.Cert, *inst.PublicIpAddress, 8080)
		if err != nil {
			return nil, fmt.Errorf("constructing node agent client: %w", err)
		}
		node := &node{
			agentClient: nodeAgentClient,
			sess:        c.Session,
			ec2Client:   c.EC2Client,
			instanceID:  *inst.InstanceId,
			accountID:   c.AccountID,
		}
		nodes = append(nodes, node)
		ifaceNodes = append(ifaceNodes, node)

	}

	err = c.waitForNodesHeartbeats(ctx, nodes)
	if err != nil {
		return nil, err
	}

	return ifaceNodes, nil
}

func (c *Cluster) waitForNodeHeartbeat(ctx context.Context, node *node) {
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

func (c *Cluster) waitForNodesHeartbeats(ctx context.Context, nodes []*node) error {
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
	nodes, err := c.NewNodes(ctx, 1)
	if err != nil {
		return nil, err
	}
	return nodes[0], nil
}

func (c *Cluster) Cleanup(ctx context.Context) error {
	return nil
}
