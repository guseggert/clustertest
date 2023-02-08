package aws

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/guseggert/clustertest/agent"
	"github.com/guseggert/clustertest/internal/files"
	"go.uber.org/zap"
)

type config struct {
	loadedMut sync.Mutex
	loaded    bool

	nodeAgentBin            string
	log                     *zap.SugaredLogger
	cert                    *agent.Certs
	session                 *session.Session
	ec2Client               *ec2.EC2
	s3Client                *s3.S3
	instanceProfileARN      string
	instanceSecurityGroupID string
	amiID                   string
	accountID               string
	subnetID                string
	nodeAgentS3Bucket       string
	nodeAgentS3Key          string
}

func (c *Cluster) ensureLoaded() error {
	c.config.loadedMut.Lock()
	defer c.config.loadedMut.Unlock()
	if c.config.loaded {
		return nil
	}

	if c.config.log == nil {
		l, err := zap.NewProduction()
		if err != nil {
			return fmt.Errorf("building logger: %w", err)
		}
		c.WithLogger(l.Sugar())
	}

	if c.config.session == nil {
		sess, err := session.NewSessionWithOptions(session.Options{SharedConfigState: session.SharedConfigEnable})
		if err != nil {
			return fmt.Errorf("creating AWS Go SDK session: %w", err)
		}
		c.config.session = sess
	}

	outputsMap, err := fetchStackOutputs(c.config.session)
	if err != nil {
		return fmt.Errorf("fetching stack outputs: %w", err)
	}

	outputs, err := parseStackOutputs(outputsMap)
	if err != nil {
		return fmt.Errorf("parsing stack outputs: %w", err)
	}

	if c.config.amiID == "" {
		amiID, err := fetchAMIID(c.config.session)
		if err != nil {
			return fmt.Errorf("fetching AMI ID: %w", err)
		}
		c.config.amiID = amiID
	}

	if c.config.cert == nil {
		cert, err := agent.GenerateCerts()
		if err != nil {
			return fmt.Errorf("generating cert: %w", err)
		}
		c.config.cert = cert
	}

	if c.config.nodeAgentBin == "" {
		nab, err := files.FindNodeAgentBin()
		if err != nil {
			return fmt.Errorf("finding node agent bin: %w", err)
		}
		c.config.nodeAgentBin = nab
	}

	c.config.s3Client = s3.New(c.config.session)
	c.config.ec2Client = ec2.New(c.config.session)

	// upload the node agent to S3
	nodeAgentKey, err := provideFileViaS3(c.config.s3Client, outputs.s3Bucket, c.config.nodeAgentBin)
	if err != nil {
		return fmt.Errorf("uploading node agent to S3: %w", err)
	}

	c.config.accountID = outputs.accountID
	c.config.instanceProfileARN = outputs.ec2InstanceProfileARN
	c.config.instanceSecurityGroupID = outputs.ec2SecurityGroupID
	c.config.subnetID = outputs.publicSubnetIDs[0]
	c.config.nodeAgentS3Bucket = outputs.s3Bucket
	c.config.nodeAgentS3Key = nodeAgentKey

	c.config.loaded = true
	return nil
}

func fetchAMIID(sess *session.Session) (string, error) {
	ssmClient := ssm.New(sess)
	key := "/aws/service/ecs/optimized-ami/amazon-linux-2/recommended"
	res, err := ssmClient.GetParameters(&ssm.GetParametersInput{Names: []*string{&key}})
	if err != nil {
		return "", fmt.Errorf("fetching AMI ID: %w", err)
	}
	val := *res.Parameters[0].Value
	m := map[string]interface{}{}
	err = json.Unmarshal([]byte(val), &m)
	if err != nil {
		return "", fmt.Errorf("unmarshaling ECS AMI info from SSM: %w", err)
	}
	amiIDIface, ok := m["image_id"]
	if !ok {
		return "", fmt.Errorf("unable to find AMI ID in SSM: %w", err)
	}
	amiID, ok := amiIDIface.(string)
	if !ok {
		return "", errors.New("expected AMI ID from SSM to be a string")
	}
	return amiID, nil
}

// provideFileViaS3 uploads the file at the path to S3 with a random key, and returns the key.
func provideFileViaS3(s3Client *s3.S3, bucket, path string) (string, error) {
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
