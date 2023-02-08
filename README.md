# clustertest

Clustertest is a framework and library for orchestrating clusters of nodes for testing. Clusters and state are transient and are destroyed at the end of the test, and cluster/node configuration is imperative and handled as part of test code.

Productivity and minimizing cognitive load are primary design goals of clustertest, and it aims to be as simple and transparent as possible.

Most importantly, clustertest has no logo.

# Use Cases

Clustertest is designed for tests that:

* Need to run code on a cluster of nodes
* Have a definitive start and end
* Need to customize/orchestrate nodes to run the test

"Nodes" can be any process--local and unsandboxed, local Docker containers, remote Docker containers, remote cloud instances, k8s containers, etc. The implementation interface for a cluster is deliberately minimal, so that cluster implementations are easy to write, and you can choose the amount of complexity you'd like.

Also since nodes do not require Docker/k8s/VMs/etc., you can run tests on bare metal machines and interact directly with e.g. networking hardware.

Because implementations are interchangeable, you can write and run your test code locally with near-instantaneous feedback, then launch the same code on a cluster of 1,000 nodes in AWS for a giant performance test. 

Clustertest also works naturally with standard test frameworks, tooling, IDE integration, etc.

# Getting Started
Here is a basic clustertest test, using the Go test framework:

```
func TestHelloWorld(t *testing.T) {
	// create a local cluster
	cluster := basic.New(local.NewCluster())

	// destroy the nodes and cluster after the test
	t.Cleanup(cluster.Cleanup)

	// create a new node
	node := cluster.MustNewNode()

	// determine the file path, which may differ depending on the implementation
	path := filepath.Join(node.RootDir(), "hello")

	// send a file to the node
	node.MustSendFile(path, bytes.NewBuffer([]byte("Hello, world!")))

	// cat the file and verify stdout
	stdout := &bytes.Buffer{}
	exitCode := node.MustRun(cluster.StartProcRequest{
		Command: "cat",
		Args:    []string{path},
		Stdout:  stdout,
	})

	assert.Equal(t, 0, exitCode)
	assert.Equal(t, "Hello, world!", stdout.String())
}
```

This same code can be run against Docker nodes by merely switching the cluster implementation:

```
// launch nodes with the default Docker image
clusterImpl := docker.NewCluster()
```

or with EC2 nodes in AWS:

```
clusterImpl := aws.NewCluster()
```

# Example Code
There are example tests in the `examples` directory.

Examples require a compiled node agent in the repo root, which you can generate with:

```
make nodeagent
```

Also check out https://github.com/guseggert/clustertest-kubo which builds functionality for testing [Kubo](https://github.com/ipfs/kubo) clusters on top of clustertest.

# Cluster Implementations
To create a new cluster implementation, you implement the Cluster and Node interfaces, which define how to create a node and cluster, and how to run code on them. Most implementations will use the "node agent" (see below), which provides an HTTP interface between the node and the test runner. These implementations should run the node agent on each node and expose its port to the test runner--then the interface implementations merely forward to the node agent client.

Existing implementations:

- Local (no sandbox)
- Local Docker containers
- AWS EC2

Potential implementations:

- AWS ECS
- GCP
- Azure
- Kubernetes
- A composition of other clusters spanning multiple clouds/datacenters/regions

## Local
Each node runs directly on the local host, with no isolation and no node agent.

For tests to be interchangeable with this and other implementations, you need to be careful to not assume that each node has its own mount namespace, network namespace, etc. E.g. two separate nodes cannot listen on the same port. If this is too complex, it's also fine to not support the local implementation.

## Local Docker
Each node runs the node agent in its own local Docker container. This requires a local Docker daemon. Launching nodes takes on the order of hundreds of milliseconds, so it is not as fast as "local" clusters but is still acceptable for many types of tests.

## AWS EC2
Each node is a full-fledged AWS EC2 instance running a node agent. Nodes take on the order of 10-30 seconds to startup, so it is only preferred for performance testing or large-scale testing. (Clustertest instantiates nodes in batches, so a 10-node cluster will still take ~30 seconds to startup, not 300 seconds).

Some basic resources need to be setup in your AWS account to run EC2 instances, such as roles, a VPC, S3 bucket, etc. This repo includes a CDK stack which will create these for you. These resources are billed by usage, so you will not be billed for deploying the CDK stack. To deploy the CDK stack:

- Install the AWS CDK: https://docs.aws.amazon.com/cdk/v2/guide/work-with.html#work-with-prerequisites
- `cd cluster/aws/cdk`
- `cdk bootstrap`
- `cdk deploy`

This creates the VPC, subnets, EC2 instance role, etc. that will be used for EC2 instances used in the tests. The tests discover these resources automatically as long as you configure them with the same account and region.

The stack needs to be deployed to each account+region you intend to use. This can be controlled using standard AWS SDK environment variables such as `AWS_PROFILE` and `AWS_DEFAULT_REGION`.

It is possible to use SSM here instead of exposing a port, but that is significantly slower.

# Node Agent
Most clustertest implementations use the "node agent", which is an HTTPS server that runs on each node in the cluster. This server handles communication between the node and the test runner, including:

* Receiving heartbeats from the test runner, and shutting down the node on heartbeat failure (if the test runner unexpectedly dies, nodes destroy themselves)
* Reading/writing files to the node's disk
* Launching processes on the node, streaming stdin from the test runner to the process, and stdout/stderr back to the test runner (this streaming happens via a WebSocket connection)
* Proxying network connections to the test runner via a WebSocket connection

The node agent uses mTLS for authn, authz, and traffic encryption, using a unique TLS cert generated by the test runner at execution time. Implementations merely need to launch the node agent and ensure there's a route to its HTTPS port.

# Questions
## What about other programming languages?
Clustertest is agnostic to the programming language of the system under test.

The default test runner implementation is Go, which means that tests themselves are written in Go.

Writing a new test runner in another language is relatively easy, though:

- Write a node agent HTTP client
- Write a test runner that launches nodes with the node agent
