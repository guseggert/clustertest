package aws

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/ssm"
)

type stackOutputs struct {
	accountID             string
	ec2InstanceProfileARN string
	ec2SecurityGroupID    string
	publicSubnetIDs       []string
	s3Bucket              string
}

type CFNResourcesProvider struct {
	CFNClient    *cloudformation.CloudFormation
	SSMClient    *ssm.SSM
	S3Client     *s3.S3
	NodeAgentBin string
	Defaults     Resources
}

func (p *CFNResourcesProvider) Provide() (*Resources, error) {
	outputsMap, err := p.fetchStackOutputs()
	if err != nil {
		return nil, fmt.Errorf("fetching stack outputs: %w", err)
	}

	outputs, err := parseStackOutputs(outputsMap)
	if err != nil {
		return nil, fmt.Errorf("parsing stack outputs: %w", err)
	}

	resources := p.Defaults

	if resources.AMIID == "" {
		amiID, err := p.fetchAMIID()
		if err != nil {
			return nil, fmt.Errorf("fetching AMI ID: %w", err)
		}
		resources.AMIID = amiID
	}

	nodeAgentKey, err := p.provideFileViaS3(outputs.s3Bucket, p.NodeAgentBin)
	if err != nil {
		return nil, fmt.Errorf("uploading node agent to S3: %w", err)
	}

	resources.NodeAgentS3Bucket = outputs.s3Bucket
	resources.NodeAgentS3Key = nodeAgentKey
	resources.SubnetID = outputs.publicSubnetIDs[0]
	resources.InstanceProfileARN = outputs.ec2InstanceProfileARN
	resources.InstanceSecurityGroupID = outputs.ec2SecurityGroupID
	resources.AccountID = outputs.accountID

	return &resources, nil
}

func parseStackOutputs(outputs map[string]string) (stackOutputs, error) {
	var stackOutputs stackOutputs
	ec2InstanceProfileARN := outputs["EC2InstanceProfileARN"]
	if ec2InstanceProfileARN == "" {
		return stackOutputs, errors.New("unable to find EC2 instance role ARN")
	}
	stackOutputs.ec2InstanceProfileARN = ec2InstanceProfileARN

	parsedARN, err := arn.Parse(ec2InstanceProfileARN)
	if err != nil {
		return stackOutputs, fmt.Errorf("parsing EC2 instance role ARN %q: %w", parsedARN, err)
	}
	stackOutputs.accountID = parsedARN.AccountID

	publicSubnetIDsStr := outputs["PublicSubnetIDs"]
	if publicSubnetIDsStr == "" {
		return stackOutputs, errors.New("unable to find subnet IDs")
	}
	stackOutputs.publicSubnetIDs = strings.Split(publicSubnetIDsStr, ",")

	securityGroupID := outputs["EC2InstanceSecurityGroupID"]
	if securityGroupID == "" {
		return stackOutputs, errors.New("unable to find security group ID")
	}
	stackOutputs.ec2SecurityGroupID = securityGroupID

	s3BucketARNStr := outputs["S3BucketARN"]
	if s3BucketARNStr == "" {
		return stackOutputs, errors.New("unable to find S3 bucket ARN")
	}
	s3BucketARN, err := arn.Parse(s3BucketARNStr)
	if err != nil {
		return stackOutputs, fmt.Errorf("parsing S3 bucket ARN %q: %w", s3BucketARNStr, err)
	}
	stackOutputs.s3Bucket = s3BucketARN.Resource

	return stackOutputs, nil
}

func (p *CFNResourcesProvider) fetchStackOutputs() (map[string]string, error) {
	listExportsPages, err := collectPages(&cloudformation.ListExportsInput{}, p.CFNClient.ListExportsPages)
	if err != nil {
		return nil, fmt.Errorf("listing CloudFormation exports: %w", err)
	}

	var testStackARN string
	for _, page := range listExportsPages {
		for _, export := range page.Exports {
			if *export.Name == "ClustertestStackARN" {
				testStackARN = *export.Value
			}
		}
	}
	if testStackARN == "" {
		return nil, errors.New("unable to find exported test stack ARN, did you run 'cdk deploy'?")
	}

	describeStacksPages, err := collectPages(
		&cloudformation.DescribeStacksInput{StackName: &testStackARN},
		p.CFNClient.DescribeStacksPages,
	)
	if len(describeStacksPages) != 1 {
		return nil, fmt.Errorf("expected DescribeStacks to have 1 page, but had %d", len(describeStacksPages))
	}
	if len(describeStacksPages[0].Stacks) != 1 {
		return nil, fmt.Errorf("expected DescribeStacks page to have 1 stack, but had %d", len(describeStacksPages[0].Stacks))
	}
	stack := describeStacksPages[0].Stacks[0]

	outputs := map[string]string{}
	for _, output := range stack.Outputs {
		outputs[*output.OutputKey] = *output.OutputValue
	}

	return outputs, nil
}

func (p *CFNResourcesProvider) fetchAMIID() (string, error) {
	key := "/aws/service/ecs/optimized-ami/amazon-linux-2/recommended"
	res, err := p.SSMClient.GetParameters(&ssm.GetParametersInput{Names: []*string{&key}})
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
func (p *CFNResourcesProvider) provideFileViaS3(bucket, path string) (string, error) {
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
	_, err = p.S3Client.PutObject(&s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   f,
	})
	if err != nil {
		return "", fmt.Errorf("putting to S3: %w", err)
	}
	return key, nil
}
