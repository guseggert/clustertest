package aws

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
)

type stackOutputs struct {
	accountID             string
	ec2InstanceProfileARN string
	ec2SecurityGroupID    string
	publicSubnetIDs       []string
	s3Bucket              string
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

func fetchStackOutputs(sess *session.Session) (map[string]string, error) {
	cfnClient := cloudformation.New(sess)
	listExportsPages, err := collectPages(&cloudformation.ListExportsInput{}, cfnClient.ListExportsPages)
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
		cfnClient.DescribeStacksPages,
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
