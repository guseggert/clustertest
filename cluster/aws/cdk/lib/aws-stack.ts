import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as s3 from 'aws-cdk-lib/aws-s3';
import { ManagedPolicy } from 'aws-cdk-lib/aws-iam';

export class AwsStack extends cdk.Stack {
    constructor(scope: Construct, id: string, props?: cdk.StackProps) {
        super(scope, id, props);

        const vpc = new ec2.Vpc(this, 'VPC', {})

        const instancePolicy = ManagedPolicy.fromAwsManagedPolicyName("AmazonSSMManagedInstanceCore");

        const instanceRole = new iam.Role(this, 'EC2InstanceRole', {
            assumedBy: new iam.ServicePrincipal('ec2.amazonaws.com'),
            managedPolicies: [instancePolicy],
        });

        const instanceProfile = new iam.CfnInstanceProfile(this, 'EC2InstanceProfile', {
            roles: [instanceRole.roleName],
        })

        const s3Bucket = new s3.Bucket(this, 'S3Bucket', {
            autoDeleteObjects: true,
            blockPublicAccess: {
                blockPublicAcls: false,
                blockPublicPolicy: false,
                ignorePublicAcls: false,
                restrictPublicBuckets: true,
            },
            lifecycleRules: [{
                enabled: true,
                expiration: cdk.Duration.days(30),
            }],
            removalPolicy: cdk.RemovalPolicy.DESTROY,
        });

        const securityGroup = new ec2.SecurityGroup(this, 'SecurityGroup', {
            vpc: vpc,
        })

        securityGroup.addIngressRule(ec2.Peer.anyIpv4(), ec2.Port.tcp(8080))
        securityGroup.addIngressRule(ec2.Peer.anyIpv6(), ec2.Port.tcp(8080))
        securityGroup.addIngressRule(ec2.Peer.anyIpv4(), ec2.Port.tcp(4001))
        securityGroup.addIngressRule(ec2.Peer.anyIpv4(), ec2.Port.udp(4001))
        securityGroup.addIngressRule(ec2.Peer.anyIpv6(), ec2.Port.tcp(4001))
        securityGroup.addIngressRule(ec2.Peer.anyIpv6(), ec2.Port.udp(4001))

        securityGroup.addIngressRule(securityGroup, ec2.Port.allTraffic())

        // Export the stack ARN, which we can use to lookup the other outputs without having to export them.
        new cdk.CfnOutput(this, "ClustertestStackARN", {
            value: this.stackId,
            exportName: "ClustertestStackARN",
        })

        new cdk.CfnOutput(this, "VPCArn", {
            value: vpc.vpcArn,
        });

        new cdk.CfnOutput(this, "PublicSubnetIDs", {
            value: vpc.publicSubnets.map(s => s.subnetId).join(","),
        })

        new cdk.CfnOutput(this, "EC2InstanceProfileARN", {
            value: instanceProfile.attrArn,
        });

        new cdk.CfnOutput(this, "EC2InstanceSecurityGroupID", {
            value: securityGroup.securityGroupId,
        })

        new cdk.CfnOutput(this, "S3BucketARN", {
            value: s3Bucket.bucketArn,
        })
    }
}
