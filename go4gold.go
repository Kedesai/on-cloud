package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/avast/retry-go"
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
		S3Bucket struct {
			Name string            `yaml:"name"`
			ACL  string            `yaml:"acl"`
			Tags map[string]string `yaml:"tags"`
		} `yaml:"s3_bucket"`
		RDSInstance struct {
			Name             string            `yaml:"name"`
			Engine           string            `yaml:"engine"`
			EngineVersion    string            `yaml:"engine_version"`
			InstanceClass    string            `yaml:"instance_class"`
			AllocatedStorage int32             `yaml:"allocated_storage"`
			Username         string            `yaml:"username"`
			Password         string            `yaml:"password"`
			Tags             map[string]string `yaml:"tags"`
		} `yaml:"rds_instance"`
		ALB struct {
			Name            string            `yaml:"name"`
			Scheme          string            `yaml:"scheme"`
			Subnets         []string          `yaml:"subnets"`
			SecurityGroups  []string          `yaml:"security_groups"`
			Listeners       []Listener        `yaml:"listeners"`
			TargetGroups    []TargetGroup     `yaml:"target_groups"`
			Tags            map[string]string `yaml:"tags"`
		} `yaml:"alb"`
		AutoScalingGroup struct {
			Name               string            `yaml:"name"`
			MinSize            int32             `yaml:"min_size"`
			MaxSize            int32             `yaml:"max_size"`
			DesiredCapacity    int32             `yaml:"desired_capacity"`
			LaunchTemplate     LaunchTemplate    `yaml:"launch_template"`
			VpcZoneIdentifier  []string          `yaml:"vpc_zone_identifier"`
			TargetGroupARNs    []string          `yaml:"target_group_arns"`
			Tags               map[string]string `yaml:"tags"`
		} `yaml:"autoscaling_group"`
	} `yaml:"resources"`
}

// LaunchTemplate represents an EC2 launch template
type LaunchTemplate struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
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

	// Validate configuration
	if cfg.Provider != "aws" {
		log.Fatalf("Unsupported provider: %s", cfg.Provider)
	}
	if cfg.Region == "" {
		log.Fatal("Region is required")
	}

	// Initialize AWS SDK
	awsCfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(cfg.Region))
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	ec2Client := ec2.NewFromConfig(awsCfg)
	s3Client := s3.NewFromConfig(awsCfg)
	rdsClient := rds.NewFromConfig(awsCfg)
	albClient := elasticloadbalancingv2.NewFromConfig(awsCfg)
	asgClient := autoscaling.NewFromConfig(awsCfg)

	// Handle EC2 Instance
	if cfg.Resources.EC2Instance.Name != "" {
		err = handleEC2Instance(ec2Client, cfg.Resources.EC2Instance)
		if err != nil {
			log.Printf("Failed to handle EC2 instance: %v", err)
		}
	}

	// Handle S3 Bucket
	if cfg.Resources.S3Bucket.Name != "" {
		err = handleS3Bucket(s3Client, cfg.Resources.S3Bucket)
		if err != nil {
			log.Printf("Failed to handle S3 bucket: %v", err)
		}
	}

	// Handle RDS Instance
	if cfg.Resources.RDSInstance.Name != "" {
		err = handleRDSInstance(rdsClient, cfg.Resources.RDSInstance)
		if err != nil {
			log.Printf("Failed to handle RDS instance: %v", err)
		}
	}

	// Handle ALB
	if cfg.Resources.ALB.Name != "" {
		err = handleALB(albClient, cfg.Resources.ALB)
		if err != nil {
			log.Printf("Failed to handle ALB: %v", err)
		}
	}

	// Handle Auto Scaling Group
	if cfg.Resources.AutoScalingGroup.Name != "" {
		err = handleAutoScalingGroup(asgClient, cfg.Resources.AutoScalingGroup)
		if err != nil {
			log.Printf("Failed to handle Auto Scaling Group: %v", err)
		}
	}
}

// handleAutoScalingGroup manages the Auto Scaling Group
func handleAutoScalingGroup(client *autoscaling.Client, desired ConfigResourcesAutoScalingGroup) error {
	asgName := desired.Name
	asg, err := findAutoScalingGroupByName(client, asgName)
	if err != nil {
		return fmt.Errorf("failed to check for existing Auto Scaling Group: %v", err)
	}

	if asg != nil {
		fmt.Printf("Auto Scaling Group already exists with name: %s\n", *asg.AutoScalingGroupName)
		fmt.Println("Current State:")
		fmt.Printf("  Min Size: %d\n", *asg.MinSize)
		fmt.Printf("  Max Size: %d\n", *asg.MaxSize)
		fmt.Printf("  Desired Capacity: %d\n", *asg.DesiredCapacity)
		fmt.Printf("  Launch Template: %s\n", *asg.LaunchTemplate.LaunchTemplateName)
		fmt.Printf("  VPC Zone Identifier: %v\n", asg.VPCZoneIdentifier)
		fmt.Printf("  Target Group ARNs: %v\n", asg.TargetGroupARNs)
		fmt.Printf("  Tags: %v\n", asg.Tags)

		changes := compareASGStates(asg, desired)
		if len(changes) > 0 {
			fmt.Println("\nChanges to be applied:")
			for _, change := range changes {
				fmt.Println(change)
			}

			fmt.Print("\nDo you want to apply these changes? (yes/no): ")
			var approval string
			fmt.Scanln(&approval)

			if approval == "yes" {
				err = updateAutoScalingGroup(client, asgName, desired)
				if err != nil {
					return fmt.Errorf("failed to update Auto Scaling Group: %v", err)
				}
				fmt.Println("Changes applied successfully.")
			} else {
				fmt.Println("Changes rejected.")
			}
		} else {
			fmt.Println("No changes required.")
		}
	} else {
		err = createAutoScalingGroup(client, desired)
		if err != nil {
			return fmt.Errorf("failed to create Auto Scaling Group: %v", err)
		}
		fmt.Printf("Created Auto Scaling Group: %s\n", asgName)
	}

	return nil
}

// findAutoScalingGroupByName checks if an Auto Scaling Group with the given name already exists
func findAutoScalingGroupByName(client *autoscaling.Client, name string) (*autoscalingtypes.AutoScalingGroup, error) {
	result, err := client.DescribeAutoScalingGroups(context.TODO(), &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{name},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe Auto Scaling Group: %v", err)
	}

	if len(result.AutoScalingGroups) > 0 {
		return &result.AutoScalingGroups[0], nil
	}

	return nil, nil
}

// compareASGStates compares the current and desired states of an Auto Scaling Group
func compareASGStates(current *autoscalingtypes.AutoScalingGroup, desired ConfigResourcesAutoScalingGroup) []string {
	var changes []string

	if *current.MinSize != desired.MinSize {
		changes = append(changes, fmt.Sprintf("Min Size: %d -> %d", *current.MinSize, desired.MinSize))
	}

	if *current.MaxSize != desired.MaxSize {
		changes = append(changes, fmt.Sprintf("Max Size: %d -> %d", *current.MaxSize, desired.MaxSize))
	}

	if *current.DesiredCapacity != desired.DesiredCapacity {
		changes = append(changes, fmt.Sprintf("Desired Capacity: %d -> %d", *current.DesiredCapacity, desired.DesiredCapacity))
	}

	if *current.LaunchTemplate.LaunchTemplateName != desired.LaunchTemplate.Name {
		changes = append(changes, fmt.Sprintf("Launch Template: %s -> %s", *current.LaunchTemplate.LaunchTemplateName, desired.LaunchTemplate.Name))
	}

	// Compare VPC Zone Identifier (simplified)
	if len(current.VPCZoneIdentifier) != len(desired.VpcZoneIdentifier) {
		changes = append(changes, fmt.Sprintf("VPC Zone Identifier: %v -> %v", current.VPCZoneIdentifier, desired.VpcZoneIdentifier))
	}

	// Compare Target Group ARNs (simplified)
	if len(current.TargetGroupARNs) != len(desired.TargetGroupARNs) {
		changes = append(changes, fmt.Sprintf("Target Group ARNs: %v -> %v", current.TargetGroupARNs, desired.TargetGroupARNs))
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

// createAutoScalingGroup creates a new Auto Scaling Group
func createAutoScalingGroup(client *autoscaling.Client, desired ConfigResourcesAutoScalingGroup) error {
	// Create the Auto Scaling Group
	_, err := client.CreateAutoScalingGroup(context.TODO(), &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(desired.Name),
		MinSize:              aws.Int32(desired.MinSize),
		MaxSize:              aws.Int32(desired.MaxSize),
		DesiredCapacity:      aws.Int32(desired.DesiredCapacity),
		LaunchTemplate: &autoscalingtypes.LaunchTemplateSpecification{
			LaunchTemplateName: aws.String(desired.LaunchTemplate.Name),
			Version:           aws.String(desired.LaunchTemplate.Version),
		},
		VPCZoneIdentifier:  aws.String(strings.Join(desired.VpcZoneIdentifier, ",")),
		TargetGroupARNs:    desired.TargetGroupARNs,
		Tags:               convertASGTags(desired.Tags),
	})
	if err != nil {
		return fmt.Errorf("failed to create Auto Scaling Group: %v", err)
	}

	return nil
}

// convertASGTags converts a map of tags to AWS Tag format
func convertASGTags(tags map[string]string) []autoscalingtypes.Tag {
	var awsTags []autoscalingtypes.Tag
	for key, value := range tags {
		awsTags = append(awsTags, autoscalingtypes.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}
	return awsTags
}