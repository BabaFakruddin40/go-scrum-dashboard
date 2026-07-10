import * as aws from "@pulumi/aws";
import * as eks from "@pulumi/eks";
import * as pulumi from "@pulumi/pulumi";

// 1. Create an AWS ECR Repository for the Go Scrum App
const repo = new aws.ecr.Repository("go-scrum-dashboard", {
    imageTagMutability: "MUTABLE",
    imageScanningConfiguration: {
        scanOnPush: true,
    },
});

// 2. Create a VPC for our EKS cluster (EKS requires isolated networks across subnets)
const vpc = new aws.ec2.Vpc("scrum-vpc", {
    cidrBlock: "10.0.0.0/16",
    enableDnsHostnames: true,
    enableDnsSupport: true,
});

// Create internet gateway to allow traffic outward
const gateway = new aws.ec2.InternetGateway("vpc-gw", {
    vpcId: vpc.id,
});

// Create 2 Public Subnets in different Availability Zones (Required by EKS)
const subnet1 = new aws.ec2.Subnet("scrum-subnet-1", {
    vpcId: vpc.id,
    cidrBlock: "10.0.1.0/24",
    availabilityZone: "us-east-1a",
    mapPublicIpOnLaunch: true,
});

const subnet2 = new aws.ec2.Subnet("scrum-subnet-2", {
    vpcId: vpc.id,
    cidrBlock: "10.0.2.0/24",
    availabilityZone: "us-east-1b",
    mapPublicIpOnLaunch: true,
});

// Route table to direct internet traffic through the gateway
const routeTable = new aws.ec2.RouteTable("scrum-rt", {
    vpcId: vpc.id,
    routes: [{
        cidrBlock: "0.0.0.0/0",
        gatewayId: gateway.id,
    }],
});

// Associate subnets with route table
new aws.ec2.RouteTableAssociation("rta-1", { subnetId: subnet1.id, routeTableId: routeTable.id });
new aws.ec2.RouteTableAssociation("rta-2", { subnetId: subnet2.id, routeTableId: routeTable.id });

// 3. Provision the Managed EKS Cluster and Worker Node Group
const cluster = new eks.Cluster("scrum-metrics-cluster", {
    vpcId: vpc.id,
    publicSubnetIds: [subnet1.id, subnet2.id],
    desiredCapacity: 2,
    minSize: 1,
    maxSize: 3,
    instanceType: "t3.medium", // Sufficient compute for Go App + Prometheus + Grafana
    providerCredentialOpts: {
        profileName: aws.config.profile,
    },
});

// 4. Export critical stack outputs
export const ecrRepositoryUrl = repo.repositoryUrl;
export const kubeconfig = cluster.kubeconfig;
export const clusterName = cluster.core.cluster.name;