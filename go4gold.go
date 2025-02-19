package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
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
	} `yaml:"resources"`
}

// Listener represents an ALB listener
type Listener struct {
	Protocol       string `yaml:"protocol"`
	Port           int32  `yaml:"port"`
	DefaultAction  Action `yaml:"default_action"`
}

// Action represents a listener action
type Action struct {
	Type          string `yaml:"type"`
	TargetGroup   string `yaml:"target_group"`
}

// TargetGroup represents an ALB target group
type TargetGroup struct {
	Name                 string `yaml:"name"`
	Protocol             string `yaml:"protocol"`
	Port                 int32  `yaml:"port"`
	HealthCheckPath      string `yaml:"health_check_path"`
	HealthCheckPort      int32  `yaml:"health_check_port"`
	HealthCheckInterval  int32  `yaml:"health_check_interval"`
	HealthCheckTimeout   int32  `yaml:"health_check_timeout"`
	HealthyThreshold     int32  `yaml:"healthy_threshold"`
	UnhealthyThreshold   int32  `yaml:"unhealthy_threshold"`
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
}

// handleALB manages the Application Load Balancer
func handleALB(client *elasticloadbalancingv2.Client, desired ConfigResourcesALB) error {
	albARN, currentState, err := findALBByName(client, desired.Name)
	if err != nil {
		return fmt.Errorf("failed to check for existing ALB: %v", err)
	}

	if albARN != "" {
		fmt.Printf("ALB already exists with ARN: %s\n", albARN)
		fmt.Println("Current State:")
		fmt.Printf("  Scheme: %s\n", *currentState.Scheme)
		fmt.Printf("  Subnets: %v\n", currentState.AvailabilityZones)
		fmt.Printf("  Security Groups: %v\n", currentState.SecurityGroups)
		fmt.Printf("  Tags: %v\n", currentState.Tags)

		changes := compareALBStates(currentState, desired)
		if len(changes) > 0 {
			fmt.Println("\nChanges to be applied:")
			for _, change := range changes {
				fmt.Println(change)
			}

			fmt.Print("\nDo you want to apply these changes? (yes/no): ")
			var approval string
			fmt.Scanln(&approval)

			if approval == "yes" {
				err = updateALB(client, albARN, desired)
				if err != nil {
					return fmt.Errorf("failed to update ALB: %v", err)
				}
				fmt.Println("Changes applied successfully.")
			} else {
				fmt.Println("Changes rejected.")
			}
		} else {
			fmt.Println("No changes required.")
		}
	} else {
		albARN, err = createALB(client, desired)
		if err != nil {
			return fmt.Errorf("failed to create ALB: %v", err)
		}
		fmt.Printf("Created ALB with ARN: %s\n", albARN)
	}

	return nil
}

// findALBByName checks if an ALB with the given name already exists
func findALBByName(client *elasticloadbalancingv2.Client, name string) (string, *elbv2types.LoadBalancer, error) {
	result, err := client.DescribeLoadBalancers(context.TODO(), &elasticloadbalancingv2.DescribeLoadBalancersInput{
		Names: []string{name},
	})
	if err != nil {
		var notFound *elbv2types.LoadBalancerNotFoundException
		if errors.As(err, &notFound) {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("failed to describe ALB: %v", err)
	}

	if len(result.LoadBalancers) > 0 {
		return *result.LoadBalancers[0].LoadBalancerArn, &result.LoadBalancers[0], nil
	}

	return "", nil, nil
}

// compareALBStates compares the current and desired states of an ALB
func compareALBStates(current *elbv2types.LoadBalancer, desired ConfigResourcesALB) []string {
	var changes []string

	if *current.Scheme != desired.Scheme {
		changes = append(changes, fmt.Sprintf("Scheme: %s -> %s", *current.Scheme, desired.Scheme))
	}

	// Compare subnets (simplified)
	if len(current.AvailabilityZones) != len(desired.Subnets) {
		changes = append(changes, fmt.Sprintf("Subnets: %v -> %v", current.AvailabilityZones, desired.Subnets))
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

// createALB creates a new Application Load Balancer
func createALB(client *elasticloadbalancingv2.Client, desired ConfigResourcesALB) (string, error) {
	// Create the ALB
	result, err := client.CreateLoadBalancer(context.TODO(), &elasticloadbalancingv2.CreateLoadBalancerInput{
		Name:           aws.String(desired.Name),
		Scheme:         elbv2types.LoadBalancerSchemeEnum(desired.Scheme),
		Subnets:        desired.Subnets,
		SecurityGroups: desired.SecurityGroups,
		Tags:           convertTags(desired.Tags),
	})
	if err != nil {
		return "", fmt.Errorf("failed to create ALB: %v", err)
	}

	albARN := *result.LoadBalancers[0].LoadBalancerArn

	// Create target groups
	for _, tg := range desired.TargetGroups {
		_, err := client.CreateTargetGroup(context.TODO(), &elasticloadbalancingv2.CreateTargetGroupInput{
			Name:       aws.String(tg.Name),
			Protocol:   elbv2types.ProtocolEnum(tg.Protocol),
			Port:       aws.Int32(tg.Port),
			VpcId:      aws.String("vpc-0123456789abcdef0"), // Replace with your VPC ID
			HealthCheckPath: aws.String(tg.HealthCheckPath),
			HealthCheckPort: aws.String(fmt.Sprintf("%d", tg.HealthCheckPort)),
			HealthCheckIntervalSeconds: aws.Int32(tg.HealthCheckInterval),
			HealthCheckTimeoutSeconds:  aws.Int32(tg.HealthCheckTimeout),
			HealthyThresholdCount:      aws.Int32(tg.HealthyThreshold),
			UnhealthyThresholdCount:    aws.Int32(tg.UnhealthyThreshold),
		})
		if err != nil {
			return "", fmt.Errorf("failed to create target group: %v", err)
		}
	}

	// Create listeners
	for _, listener := range desired.Listeners {
		targetGroupARN := fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:targetgroup/%s", desired.Region, "123456789012", listener.DefaultAction.TargetGroup)
		_, err := client.CreateListener(context.TODO(), &elasticloadbalancingv2.CreateListenerInput{
			LoadBalancerArn: aws.String(albARN),
			Protocol:        elbv2types.ProtocolEnum(listener.Protocol),
			Port:            aws.Int32(listener.Port),
			DefaultActions: []elbv2types.Action{
				{
					Type: elbv2types.ActionTypeEnum(listener.DefaultAction.Type),
					TargetGroupArn: aws.String(targetGroupARN),
				},
			},
		})
		if err != nil {
			return "", fmt.Errorf("failed to create listener: %v", err)
		}
	}

	return albARN, nil
}

// convertTags converts a map of tags to AWS Tag format
func convertTags(tags map[string]string) []elbv2types.Tag {
	var awsTags []elbv2types.Tag
	for key, value := range tags {
		awsTags = append(awsTags, elbv2types.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}
	return awsTags
}