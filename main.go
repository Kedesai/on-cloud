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
type MainConfig struct {
	Provider  string `yaml:"provider"`
	Region    string `yaml:"region"`
	Resources struct {
		EC2Instance EC2InstanceConfig `yaml:"ec2_instance"`
	} `yaml:"resources"`
	SubnetID string `yaml:"subnet_id"` // SubnetID is now in MainConfig
}

// EC2InstanceConfig represents the EC2 instance configuration
type EC2InstanceConfig struct {
	Name                string            `yaml:"name"`
	InstanceType        string            `yaml:"instance_type"`
	AMI                 string            `yaml:"ami"`
	KeyName             string            `yaml:"key_name"`
	Tags                map[string]string `yaml:"tags"`
	VPCSecurityGroupIDs []string          `yaml:"vpc_security_group_ids"`
	Monitoring          bool              `yaml:"monitoring"`
	DesiredCount        int               `yaml:"desired_count"` // Number of instances to maintain
	OS                  string            `yaml:"os"`
}

// Variables represents the variable file
type EC2Variables struct {
	Region      string `yaml:"region"`
	EC2Instance struct {
		KeyName             string   `yaml:"key_name"`
		VPCSecurityGroupIDs []string `yaml:"vpc_security_group_ids"`
		Monitoring          bool     `yaml:"monitoring"`
		DesiredCount        int      `yaml:"desired_count"`
		OS                  string   `yaml:"os"`
		SubnetID            string   `yaml:"subnet_id"` // SubnetID is now in EC2Variables
	} `yaml:"ec2_instance"`
}

// AMI mapping for different regions and OSes
var amiMap = map[string]map[string]string{
	"us-east-1": {
		"amazonLinux2": "ami-045602374a1982480", // Example: Amazon Linux 2 (20240412)
		// Add other OSes for us-east-1 if needed.
	},
	// Add more regions and OSes as needed.
}

func main() {
	mainHandler()
}

func mainHandler() {
	// Load YAML configuration
	cfg, err := loadConfig("infra.yaml")
	if err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	// Load variables from variables.yaml (if it exists)
	vars, err := loadVariables("variables.yaml")
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("Error loading variables: %v", err)
	}

	// Merge variables into the main configuration
	mergeVariables(cfg, vars)

	// Update the AMI based on the region and OS
	if ami, ok := amiMap[cfg.Region][cfg.Resources.EC2Instance.OS]; ok {
		cfg.Resources.EC2Instance.AMI = ami
	} else {
		log.Fatalf("No AMI found for region %s and OS %s", cfg.Region, cfg.Resources.EC2Instance.OS)
	}

	// Validate configuration
	validateConfig(cfg)

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
	fmt.Printf("Found %d instances.\n", len(existingInstances))
	fmt.Printf("Desired number of instances: %d\n", cfg.Resources.EC2Instance.DesiredCount)

	if len(existingInstances) == cfg.Resources.EC2Instance.DesiredCount {
		fmt.Println("No action needed. The desired number of instances already exist.")
		return
	}

	// Calculate how many instances need to be created or terminated
	instancesToCreate := cfg.Resources.EC2Instance.DesiredCount - len(existingInstances)

	if instancesToCreate > 0 {
		fmt.Printf("Creating %d new EC2 instances...\n", instancesToCreate)
		// Create the required number of instances
		for i := 0; i < instancesToCreate; i++ {
			fmt.Printf("Creating instance %d of %d...\n", i+1, instancesToCreate)
			// Use cfg.SubnetID here
			err := createEC2InstanceWithRetry(ec2Client, cfg.Resources.EC2Instance, cfg.SubnetID, cfg.Resources.EC2Instance.Name)
			if err != nil {
				log.Fatalf("Failed to create EC2 instance: %v", err)
			}
		}
	} else if instancesToCreate < 0 {
		// Terminate excess instances
		instancesToTerminate := -instancesToCreate
		fmt.Printf("Terminating %d excess EC2 instances...\n", instancesToTerminate)
		err := terminateExcessInstances(ec2Client, existingInstances, instancesToTerminate)
		if err != nil {
			log.Fatalf("Failed to terminate EC2 instances: %v", err)
		}
	}

	fmt.Println("EC2 instance creation/termination completed successfully.")
}

// loadConfig loads the main configuration from a YAML file.
func loadConfig(filename string) (*MainConfig, error) {
	configFile, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read YAML file: %v", err)
	}

	var cfg MainConfig
	err = yaml.Unmarshal(configFile, &cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %v", err)
	}
	return &cfg, nil
}

// loadVariables loads the variables from a YAML file.
func loadVariables(filename string) (*EC2Variables, error) {
	varsFile, err := os.ReadFile(filename)
	if err != nil {
		return nil, err // Let the caller handle the error (e.g., file not found)
	}

	var vars EC2Variables
	err = yaml.Unmarshal(varsFile, &vars)
	if err != nil {
		return nil, fmt.Errorf("failed to parse variables file: %v", err)
	}
	return &vars, nil
}

// mergeVariables merges variables into the main configuration.
func mergeVariables(cfg *MainConfig, vars *EC2Variables) {
	if vars == nil {
		return
	}
	if cfg.Region == "" && vars.Region != "" {
		cfg.Region = vars.Region
	}
	if cfg.Resources.EC2Instance.KeyName == "" && vars.EC2Instance.KeyName != "" {
		cfg.Resources.EC2Instance.KeyName = vars.EC2Instance.KeyName
	}
	if len(cfg.Resources.EC2Instance.VPCSecurityGroupIDs) == 0 && len(vars.EC2Instance.VPCSecurityGroupIDs) > 0 {
		cfg.Resources.EC2Instance.VPCSecurityGroupIDs = vars.EC2Instance.VPCSecurityGroupIDs
	}
	if !cfg.Resources.EC2Instance.Monitoring && vars.EC2Instance.Monitoring {
		cfg.Resources.EC2Instance.Monitoring = vars.EC2Instance.Monitoring
	}
	if cfg.Resources.EC2Instance.DesiredCount == 0 && vars.EC2Instance.DesiredCount > 0 {
		cfg.Resources.EC2Instance.DesiredCount = vars.EC2Instance.DesiredCount
	}
	if cfg.Resources.EC2Instance.OS == "" && vars.EC2Instance.OS != "" {
		cfg.Resources.EC2Instance.OS = vars.EC2Instance.OS
	}

	// Correctly match the subnet id
	if cfg.SubnetID == "" && vars.EC2Instance.SubnetID != "" {
		cfg.SubnetID = vars.EC2Instance.SubnetID
	}
}

// validateConfig validates the main configuration.
func validateConfig(cfg *MainConfig) {
	if cfg.Provider != "aws" {
		log.Fatalf("Unsupported provider: %s", cfg.Provider)
	}
	if cfg.Region == "" {
		log.Fatal("Region is required")
	}
	if cfg.Resources.EC2Instance.DesiredCount <= 0 {
		log.Fatal("DesiredCount must be greater than 0")
	}
	if cfg.Resources.EC2Instance.OS == "" {
		log.Fatal("OS must be set")
	}
	if cfg.Resources.EC2Instance.Name == "" {
		log.Fatal("Name must be set")
	}
	if len(cfg.Resources.EC2Instance.VPCSecurityGroupIDs) == 0 {
		log.Fatal("VPCSecurityGroupIDs must be set")
	}
	if cfg.SubnetID == "" {
		log.Fatal("SubnetID must be set")
	}
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
				Values: []string{"running", "pending", "stopping", "stopped"},
			},
		},
	}

	result, err := client.DescribeInstances(context.TODO(), input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %v", err)
	}

	var instances []types.Instance
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			// Check if the instance has the correct Name tag
			for _, tag := range instance.Tags {
				if *tag.Key == "Name" && *tag.Value == name {
					instances = append(instances, instance)
					break
				}
			}
		}
	}

	return instances, nil
}

// createEC2Instance : This function is responsible for creating an ec2 instance
func createEC2Instance(client *ec2.Client, instanceConfig EC2InstanceConfig, SubnetID, instanceName string) error {
	if SubnetID == "" {
		return fmt.Errorf("subnet ID is required")
	}
	if len(instanceConfig.VPCSecurityGroupIDs) == 0 {
		return fmt.Errorf("VPC security group IDs are required")
	}
	if instanceConfig.AMI == "" {
		return fmt.Errorf("AMI is required")
	}

	// Convert the tags map into a slice of types.Tag
	tags := convertTags(instanceConfig.Tags)

	// Add the Name tag to the list of tags
	tags = append(tags, types.Tag{
		Key:   aws.String("Name"),
		Value: aws.String(instanceName),
	})

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String(instanceConfig.AMI),
		InstanceType: types.InstanceType(instanceConfig.InstanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		KeyName:      aws.String(instanceConfig.KeyName),
		Monitoring: &types.RunInstancesMonitoringEnabled{
			Enabled: aws.Bool(instanceConfig.Monitoring),
		},
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{
			{
				DeviceIndex:              aws.Int32(0),
				SubnetId:                 aws.String(SubnetID),
				Groups:                   instanceConfig.VPCSecurityGroupIDs,
				AssociatePublicIpAddress: aws.Bool(true),
			},
		},
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         tags, // Apply the tags here
			},
		},
	}

	_, err := client.RunInstances(context.TODO(), input)
	if err != nil {
		log.Printf("Error creating EC2 instance: %v", err)
		return fmt.Errorf("failed to create EC2 instance: %v", err)
	}

	fmt.Printf("EC2 instance '%s' created successfully.\n", instanceConfig.Name)
	return nil
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

// createEC2InstanceWithRetry retries the EC2 instance creation if it fails
func createEC2InstanceWithRetry(client *ec2.Client, instanceConfig EC2InstanceConfig, SubnetID, instanceName string) error {
	return retry.Do(
		func() error {
			// Use cfg.SubnetID instead of instanceConfig.SubnetID.
			return createEC2Instance(client, instanceConfig, SubnetID, instanceName)
		},
		retry.Attempts(3),          // Retry 3 times
		retry.Delay(2*time.Second), // Delay between retries
	)
}

func terminateExcessInstances(client *ec2.Client, instances []types.Instance, count int) error {
	if count <= 0 {
		return nil
	}

	instanceIDs := make([]string, 0, count)
	for i := 0; i < count; i++ {
		if i < len(instances) {
			instanceIDs = append(instanceIDs, *instances[i].InstanceId)
		}
	}

	input := &ec2.TerminateInstancesInput{
		InstanceIds: instanceIDs,
	}

	_, err := client.TerminateInstances(context.TODO(), input)
	if err != nil {
		return fmt.Errorf("failed to terminate instances: %v", err)
	}

	fmt.Printf("Requested termination of %d instances: %v\n", count, instanceIDs)
	return nil
}

// findExistingInstanceWithSameConfig: This function finds an existing instance with the same configuration.
func findExistingInstanceWithSameConfig(client *ec2.Client, instanceConfig EC2InstanceConfig, instanceName, subnetId string) (*types.Instance, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: []string{instanceName},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"running", "pending", "stopping", "stopped"},
			},
			{
				Name:   aws.String("instance-type"),
				Values: []string{instanceConfig.InstanceType},
			},
			{
				Name:   aws.String("image-id"),
				Values: []string{instanceConfig.AMI},
			},
			{
				Name:   aws.String("key-name"),
				Values: []string{instanceConfig.KeyName},
			},
			{
				Name:   aws.String("subnet-id"),
				Values: []string{subnetId},
			},
		},
	}

	result, err := client.DescribeInstances(context.TODO(), input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %v", err)
	}

	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			//Checking the vpc security group ids
			if len(instance.SecurityGroups) != len(instanceConfig.VPCSecurityGroupIDs) {
				continue
			}
			allGroupsMatched := true
			for _, securityGroup := range instance.SecurityGroups {
				found := false
				for _, configGroup := range instanceConfig.VPCSecurityGroupIDs {
					if *securityGroup.GroupId == configGroup {
						found = true
						break
					}
				}
				if !found {
					allGroupsMatched = false
					break
				}
			}
			if allGroupsMatched {
				return &instance, nil
			}
		}
	}

	return nil, nil // No matching instance found
}
