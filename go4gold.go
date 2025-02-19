package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"gopkg.in/yaml.v3"
)

// Config represents the YAML configuration
type Config struct {
	Provider string `yaml:"provider"`
	Region   string `yaml:"region"`
	Resources struct {
		EC2Instance struct {
			Name           string            `yaml:"name"`
			InstanceType   string            `yaml:"instance_type"`
			AMI            string            `yaml:"ami"`
			KeyName        string            `yaml:"key_name"`
			SecurityGroups []string          `yaml:"security_groups"`
			Tags           map[string]string `yaml:"tags"`
		} `yaml:"ec2_instance"`
	} `yaml:"resources"`
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

	// Initialize AWS SDK
	awsCfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(cfg.Region))
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	ec2Client := ec2.NewFromConfig(awsCfg)

	// Check if the EC2 instance already exists
	instanceID, currentState, err := findInstanceByName(ec2Client, cfg.Resources.EC2Instance.Name)
	if err != nil {
		log.Fatalf("Failed to check for existing instance: %v", err)
	}

	if instanceID != "" {
		fmt.Printf("Instance already exists with ID: %s\n", instanceID)
		fmt.Println("Current State:")
		fmt.Printf("  Instance Type: %s\n", currentState.InstanceType)
		fmt.Printf("  AMI: %s\n", currentState.ImageId)
		fmt.Printf("  Key Name: %s\n", currentState.KeyName)
		fmt.Printf("  Security Groups: %v\n", currentState.SecurityGroups)
		fmt.Printf("  Tags: %v\n", currentState.Tags)

		// Compare current state with desired state
		desiredState := cfg.Resources.EC2Instance
		changes := compareStates(currentState, desiredState)

		if len(changes) > 0 {
			fmt.Println("\nChanges to be applied:")
			for _, change := range changes {
				fmt.Println(change)
			}

			// Ask for approval
			fmt.Print("\nDo you want to apply these changes? (yes/no): ")
			var approval string
			fmt.Scanln(&approval)

			if approval == "yes" {
				// Apply changes
				err = updateEC2Instance(ec2Client, instanceID, desiredState)
				if err != nil {
					log.Fatalf("Failed to update EC2 instance: %v", err)
				}
				fmt.Println("Changes applied successfully.")
			} else {
				fmt.Println("Changes rejected.")
			}
		} else {
			fmt.Println("No changes required.")
		}
	} else {
		// Create the EC2 instance
		instanceID, err = createEC2Instance(ec2Client, cfg.Resources.EC2Instance)
		if err != nil {
			log.Fatalf("Failed to create EC2 instance: %v", err)
		}
		fmt.Printf("Created EC2 instance with ID: %s\n", instanceID)
	}
}

// findInstanceByName checks if an EC2 instance with the given name already exists
func findInstanceByName(client *ec2.Client, name string) (string, *types.Instance, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: []string{name},
			},
		},
	}

	result, err := client.DescribeInstances(context.TODO(), input)
	if err != nil {
		return "", nil, err
	}

	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			return *instance.InstanceId, &instance, nil
		}
	}

	return "", nil, nil
}

// compareStates compares the current state with the desired state and returns a list of changes
func compareStates(current *types.Instance, desired ConfigResourcesEC2Instance) []string {
	var changes []string

	if *current.InstanceType != desired.InstanceType {
		changes = append(changes, fmt.Sprintf("Instance Type: %s -> %s", *current.InstanceType, desired.InstanceType))
	}

	if *current.ImageId != desired.AMI {
		changes = append(changes, fmt.Sprintf("AMI: %s -> %s", *current.ImageId, desired.AMI))
	}

	if *current.KeyName != desired.KeyName {
		changes = append(changes, fmt.Sprintf("Key Name: %s -> %s", *current.KeyName, desired.KeyName))
	}

	// Compare security groups (simplified)
	if len(current.SecurityGroups) != len(desired.SecurityGroups) {
		changes = append(changes, fmt.Sprintf("Security Groups: %v -> %v", current.SecurityGroups, desired.SecurityGroups))
	}

	// Compare tags (simplified)
	currentTags := make(map[string]string)
	for _, tag := range current.Tags {
		currentTags[*tag.Key] = *tag.Value
	}
	for key, value := range desired.Tags {
		if currentTags[key] != value {
			changes = append(changes, fmt.Sprintf("Tag %s: %s -> %s", key, currentTags[key], value))
		}
	}

	return changes
}

// updateEC2Instance updates an existing EC2 instance to match the desired state
func updateEC2Instance(client *ec2.Client, instanceID string, desired ConfigResourcesEC2Instance) error {
	// Stop the instance (required to change instance type)
	_, err := client.StopInstances(context.TODO(), &ec2.StopInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("failed to stop instance: %v", err)
	}

	// Wait for the instance to stop
	waiter := ec2.NewInstanceStoppedWaiter(client)
	err = waiter.Wait(context.TODO(), &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, 5*60) // 5-minute timeout
	if err != nil {
		return fmt.Errorf("failed to wait for instance to stop: %v", err)
	}

	// Modify instance attributes
	_, err = client.ModifyInstanceAttribute(context.TODO(), &ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		InstanceType: &types.AttributeValue{
			Value: aws.String(desired.InstanceType),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to modify instance type: %v", err)
	}

	// Start the instance
	_, err = client.StartInstances(context.TODO(), &ec2.StartInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("failed to start instance: %v", err)
	}

	return nil
}

// createEC2Instance creates a new EC2 instance
func createEC2Instance(client *ec2.Client, instanceConfig ConfigResourcesEC2Instance) (string, error) {
	// Prepare tags
	var tags []types.Tag
	for key, value := range instanceConfig.Tags {
		tags = append(tags, types.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}
	tags = append(tags, types.Tag{
		Key:   aws.String("Name"),
		Value: aws.String(instanceConfig.Name),
	})

	// Create the instance
	input := &ec2.RunInstancesInput{
		ImageId:          aws.String(instanceConfig.AMI),
		InstanceType:     types.InstanceType(instanceConfig.InstanceType),
		KeyName:          aws.String(instanceConfig.KeyName),
		SecurityGroups:   instanceConfig.SecurityGroups,
		MinCount:         aws.Int32(1),
		MaxCount:         aws.Int32(1),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         tags,
			},
		},
	}

	result, err := client.RunInstances(context.TODO(), input)
	if err != nil {
		return "", err
	}

	return *result.Instances[0].InstanceId, nil
}