package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/avast/retry-go"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"gopkg.in/yaml.v3"
)

// Config represents the YAML configuration
type Config struct {
	Provider  string `yaml:"provider"`
	Region    string `yaml:"region"`
	Resources struct {
		EC2Instance EC2InstanceConfig `yaml:"ec2_instance"`
	} `yaml:"resources"`
}

// EC2InstanceConfig represents the EC2 instance configuration
type EC2InstanceConfig struct {
	Name                string            `yaml:"name"`
	InstanceType        string            `yaml:"instance_type"`
	AMI                 string            `yaml:"ami"`
	KeyName             string            `yaml:"key_name"`
	SecurityGroups      []string          `yaml:"security_groups"`
	SubnetID            string            `yaml:"subnet_id"`
	VPCSecurityGroupIDs []string          `yaml:"vpc_security_group_ids"`
	Monitoring          bool              `yaml:"monitoring"`
	Tags                map[string]string `yaml:"tags"`
	DesiredCount        int               `yaml:"desired_count"` // Number of instances to maintain
}

// Variables represents the variable file
type Variables struct {
	Region      string `yaml:"region"`
	EC2Instance struct {
		KeyName             string   `yaml:"key_name"`
		AMI                 string   `yaml:"ami"`
		SecurityGroups      []string `yaml:"security_groups"`
		SubnetID            string   `yaml:"subnet_id"`
		VPCSecurityGroupIDs []string `yaml:"vpc_security_group_ids"`
		Monitoring          bool     `yaml:"monitoring"`
		DesiredCount        int      `yaml:"desired_count"`
	} `yaml:"ec2_instance"`
}

func main() {
	// Load YAML configuration
	configFile, err := os.ReadFile("infra.yaml")
	if err != nil {
		log.Fatalf("Failed to read YAML file: %v", err)
	}

	var cfg Config
	err = yaml.Unmarshal(configFile, &cfg)
	if err != nil {
		log.Fatalf("Failed to parse YAML: %v", err)
	}

	// Load variables from variables.yaml (if it exists)
	var vars Variables
	varsFile, err := os.ReadFile("variables.yaml")
	if err == nil {
		err = yaml.Unmarshal(varsFile, &vars)
		if err != nil {
			log.Fatalf("Failed to parse variables file: %v", err)
		}
	}

	// Merge variables into the main configuration
	if vars.Region != "" {
		cfg.Region = vars.Region
	}
	if vars.EC2Instance.KeyName != "" {
		cfg.Resources.EC2Instance.KeyName = vars.EC2Instance.KeyName
	}
	if vars.EC2Instance.AMI != "" {
		cfg.Resources.EC2Instance.AMI = vars.EC2Instance.AMI
	}
	if len(vars.EC2Instance.SecurityGroups) > 0 {
		cfg.Resources.EC2Instance.SecurityGroups = vars.EC2Instance.SecurityGroups
	}
	if vars.EC2Instance.SubnetID != "" {
		cfg.Resources.EC2Instance.SubnetID = vars.EC2Instance.SubnetID
	}
	if len(vars.EC2Instance.VPCSecurityGroupIDs) > 0 {
		cfg.Resources.EC2Instance.VPCSecurityGroupIDs = vars.EC2Instance.VPCSecurityGroupIDs
	}
	if vars.EC2Instance.Monitoring {
		cfg.Resources.EC2Instance.Monitoring = vars.EC2Instance.Monitoring
	}
	if vars.EC2Instance.DesiredCount > 0 {
		cfg.Resources.EC2Instance.DesiredCount = vars.EC2Instance.DesiredCount
	}

	// Validate configuration
	if cfg.Provider != "aws" {
		log.Fatalf("Unsupported provider: %s", cfg.Provider)
	}
	if cfg.Region == "" {
		log.Fatal("Region is required")
	}
	if cfg.Resources.EC2Instance.DesiredCount <= 0 {
		log.Fatal("DesiredCount must be greater than 0")
	}

	// Initialize AWS SDK
	awsCfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(cfg.Region))
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	ec2Client := ec2.NewFromConfig(awsCfg)

	// Check if the desired number of instances already exist
	existingInstances, err := getExistingInstances(ec2Client, cfg.Resources.EC2Instance.Name)
	if err != nil {
		log.Fatalf("Failed to check existing instances: %v", err)
	}

	// Print what the script is going to do
	fmt.Printf("Checking existing EC2 instances with the name '%s'...\n", cfg.Resources.EC2Instance.Name)
	fmt.Printf("Found %d instances already running or pending.\n", len(existingInstances))
	fmt.Printf("Desired number of instances: %d\n", cfg.Resources.EC2Instance.DesiredCount)

	if len(existingInstances) >= cfg.Resources.EC2Instance.DesiredCount {
		fmt.Println("No action needed. The desired number of instances already exist.")
		return
	}

	// Calculate how many instances need to be created
	instancesToCreate := cfg.Resources.EC2Instance.DesiredCount - len(existingInstances)
	fmt.Printf("Creating %d new EC2 instances...\n", instancesToCreate)

	// Create the required number of instances
	for i := 0; i < instancesToCreate; i++ {
		fmt.Printf("Creating instance %d of %d...\n", i+1, instancesToCreate)
		err := handleEC2InstanceWithRetry(ec2Client, cfg.Resources.EC2Instance)
		if err != nil {
			log.Fatalf("Failed to create EC2 instance: %v", err)
		}
	}

	fmt.Println("EC2 instance creation completed successfully.")
}

// getExistingInstances checks for existing instances with the given name tag
func getExistingInstances(client *ec2.Client, name string) ([]types.Instance, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: []string{name},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"running", "pending"},
			},
		},
	}

	result, err := client.DescribeInstances(context.TODO(), input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %v", err)
	}

	var instances []types.Instance
	for _, reservation := range result.Reservations {
		instances = append(instances, reservation.Instances...)
	}

	return instances, nil
}

func handleEC2Instance(client *ec2.Client, instanceConfig EC2InstanceConfig) error {
	if instanceConfig.SubnetID == "" {
		return fmt.Errorf("subnet ID is required")
	}
	if len(instanceConfig.VPCSecurityGroupIDs) == 0 {
		return fmt.Errorf("VPC security group IDs are required")
	}

	input := &ec2.RunInstancesInput{
		ImageId:          aws.String(instanceConfig.AMI),
		InstanceType:     types.InstanceType(instanceConfig.InstanceType),
		MinCount:         aws.Int32(1),
		MaxCount:         aws.Int32(1),
		KeyName:          aws.String(instanceConfig.KeyName),
		SecurityGroupIds: instanceConfig.SecurityGroups,
		SubnetId:         aws.String(instanceConfig.SubnetID),
		Monitoring: &types.RunInstancesMonitoringEnabled{
			Enabled: aws.Bool(instanceConfig.Monitoring),
		},
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{
			{
				DeviceIndex:              aws.Int32(0),
				SubnetId:                 aws.String(instanceConfig.SubnetID),
				Groups:                   instanceConfig.VPCSecurityGroupIDs,
				AssociatePublicIpAddress: aws.Bool(true),
			},
		},
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         convertTags(instanceConfig.Tags),
			},
		},
	}

	_, err := client.RunInstances(context.TODO(), input)
	if err != nil {
		return fmt.Errorf("failed to create EC2 instance: %v", err)
	}

	fmt.Printf("EC2 instance '%s' created successfully.\n", instanceConfig.Name)
	return nil
}

func handleEC2InstanceWithRetry(client *ec2.Client, instanceConfig EC2InstanceConfig) error {
	return retry.Do(
		func() error {
			return handleEC2Instance(client, instanceConfig)
		},
		retry.Attempts(3),          // Retry 3 times
		retry.Delay(2*time.Second), // Delay between retries
	)
}

func convertTags(tags map[string]string) []types.Tag {
	var result []types.Tag
	for key, value := range tags {
		result = append(result, types.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}
	return result
}
