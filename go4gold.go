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

	// Use a WaitGroup to wait for all goroutines to complete
	var wg sync.WaitGroup
	errChan := make(chan error, 5) // Buffer for 5 errors (one per resource type)

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

	// Handle S3 Bucket
	if cfg.Resources.S3Bucket.Name != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := handleS3Bucket(s3Client, cfg.Resources.S3Bucket)
			if err != nil {
				errChan <- fmt.Errorf("S3 bucket error: %v", err)
			}
		}()
	}

	// Handle RDS Instance
	if cfg.Resources.RDSInstance.Name != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := handleRDSInstance(rdsClient, cfg.Resources.RDSInstance)
			if err != nil {
				errChan <- fmt.Errorf("RDS instance error: %v", err)
			}
		}()
	}

	// Handle ALB
	if cfg.Resources.ALB.Name != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := handleALB(albClient, cfg.Resources.ALB)
			if err != nil {
				errChan <- fmt.Errorf("ALB error: %v", err)
			}
		}()
	}

	// Handle Auto Scaling Group
	if cfg.Resources.AutoScalingGroup.Name != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := handleAutoScalingGroup(asgClient, cfg.Resources.AutoScalingGroup)
			if err != nil {
				errChan <- fmt.Errorf("Auto Scaling Group error: %v", err)
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