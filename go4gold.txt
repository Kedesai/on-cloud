package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
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
	"github.com/aws/aws-sdk-go-v2/service/iam"
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
		IAMRole struct {
			Name             string `yaml:"name"`
			AssumeRolePolicy string `yaml:"assume_role_policy"`
			Policies         []struct {
				Name   string `yaml:"name"`
				Policy string `yaml:"policy"`
			} `yaml:"policies"`
		} `yaml:"iam_role"`
	} `yaml:"resources"`
}

// Variables represents the variable file
type Variables struct {
	Region string `yaml:"region"`
	EC2Instance struct {
		KeyName             string   `yaml:"key_name"`
		AMI                 string   `yaml:"ami"`
		SecurityGroups      []string `yaml:"security_groups"`
		SubnetID            string   `yaml:"subnet_id"`
		VPCSecurityGroupIDs []string `yaml:"vpc_security_group_ids"`
		Monitoring          bool     `yaml:"monitoring"`
	} `yaml:"ec2_instance"`
	RDSInstance struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"rds_instance"`
	ALB struct {
		SecurityGroups []string `yaml:"security_groups"`
	} `yaml:"alb"`
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
	iamClient := iam.NewFromConfig(awsCfg)

	// Use a WaitGroup to wait for all goroutines to complete
	var wg sync.WaitGroup
	errChan := make(chan error, 2) // Buffer for errors (one per resource type)

	// Handle EC2 Instance
	if cfg.Resources.EC2Instance.Name != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := handleEC2Instance(ec2Client, cfg.Resources.EC2Instance)
			if err != nil {
				errChan <- fmt.Errorf("EC2 instance error: %v", err)
			}
		}()
	}

	// Handle IAM Role
	if cfg.Resources.IAMRole.Name != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := handleIAMRole(iamClient, cfg)
			if err != nil {
				errChan <- fmt.Errorf("IAM role error: %v", err)
			}
		}()
	}

	// Wait for all goroutines to complete
	wg.Wait()
	close(errChan) // Close the error channel

	// Collect and log errors
	for err := range errChan {
		log.Println(err)
	}
}

func handleEC2Instance(client *ec2.Client, instanceConfig Config.Resources.EC2Instance) error {
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
				DeviceIndex:         aws.Int32(0),
				SubnetId:           aws.String(instanceConfig.SubnetID),
				Groups:             instanceConfig.VPCSecurityGroupIDs,
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

	log.Printf("EC2 instance '%s' created successfully", instanceConfig.Name)
	return nil
}

func handleIAMRole(client *iam.Client, cfg Config) error {
	// Create the IAM role
	createRoleInput := &iam.CreateRoleInput{
		RoleName:                 aws.String(cfg.Resources.IAMRole.Name),
		AssumeRolePolicyDocument: aws.String(cfg.Resources.IAMRole.AssumeRolePolicy),
	}

	_, err := client.CreateRole(context.TODO(), createRoleInput)
	if err != nil {
		return fmt.Errorf("failed to create IAM role: %v", err)
	}

	// Attach policies to the role
	for _, policy := range cfg.Resources.IAMRole.Policies {
		putPolicyInput := &iam.PutRolePolicyInput{
			RoleName:       aws.String(cfg.Resources.IAMRole.Name),
			PolicyName:     aws.String(policy.Name),
			PolicyDocument: aws.String(policy.Policy),
		}

		_, err := client.PutRolePolicy(context.TODO(), putPolicyInput)
		if err != nil {
			return fmt.Errorf("failed to attach policy %s: %v", policy.Name, err)
		}
	}

	log.Printf("IAM role '%s' created successfully", cfg.Resources.IAMRole.Name)
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