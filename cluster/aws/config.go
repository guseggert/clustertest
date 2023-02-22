package aws

import (
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/guseggert/clustertest/agent"
	"github.com/guseggert/clustertest/internal/files"
	"go.uber.org/zap"
)

// config loads and stores dynamically-loaded config for the cluster.
type config struct {
	loadedMut sync.Mutex
	loaded    bool

	resourcesProvider func() (*Resources, error)
	resources         *Resources

	nodeAgentBin string
	log          *zap.SugaredLogger
	cert         *agent.Certs
	session      *session.Session
	ec2Client    *ec2.EC2
	s3Client     *s3.S3
}

type ResourcesProvider func() (*Resources, error)

// Resources contains the AWS resources required for running an EC2 cluster.
type Resources struct {
	InstanceProfileARN      string
	InstanceSecurityGroupID string
	AMIID                   string
	AccountID               string
	SubnetID                string
	NodeAgentS3Bucket       string
	NodeAgentS3Key          string
}

func (c *config) withLogger(l *zap.SugaredLogger) *config {
	c.log = l
	return c
}

func (c *config) ensureLoaded() error {
	c.loadedMut.Lock()
	defer c.loadedMut.Unlock()
	if c.loaded {
		return nil
	}

	if c.log == nil {
		l, err := zap.NewProduction()
		if err != nil {
			return fmt.Errorf("building logger: %w", err)
		}
		c.withLogger(l.Sugar())
	}

	if c.session == nil {
		sess, err := session.NewSessionWithOptions(session.Options{SharedConfigState: session.SharedConfigEnable})
		if err != nil {
			return fmt.Errorf("creating AWS Go SDK session: %w", err)
		}
		c.session = sess
	}

	if c.cert == nil {
		cert, err := agent.GenerateCerts()
		if err != nil {
			return fmt.Errorf("generating cert: %w", err)
		}
		c.cert = cert
	}

	if c.nodeAgentBin == "" {
		nab, err := files.FindNodeAgentBin()
		if err != nil {
			return fmt.Errorf("finding node agent bin: %w", err)
		}
		c.nodeAgentBin = nab
	}

	c.s3Client = s3.New(c.session)
	c.ec2Client = ec2.New(c.session)

	if c.resourcesProvider == nil {
		provider := &CFNResourcesProvider{
			S3Client:     c.s3Client,
			CFNClient:    cloudformation.New(c.session),
			SSMClient:    ssm.New(c.session),
			NodeAgentBin: c.nodeAgentBin,
		}
		c.resourcesProvider = provider.Provide
	}

	resources, err := c.resourcesProvider()
	if err != nil {
		return err
	}
	c.resources = resources

	c.loaded = true
	return nil
}
