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
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
	s3Client := s3.NewFromConfig(awsCfg)
	rdsClient := rds.NewFromConfig(awsCfg)

	// Handle EC2 Instance
	if cfg.Resources.EC2Instance.Name != "" {
		handleEC2Instance(ec2Client, cfg.Resources.EC2Instance)
	}

	// Handle S3 Bucket
	if cfg.Resources.S3Bucket.Name != "" {
		handleS3Bucket(s3Client, cfg.Resources.S3Bucket)
	}

	// Handle RDS Instance
	if cfg.Resources.RDSInstance.Name != "" {
		handleRDSInstance(rdsClient, cfg.Resources.RDSInstance)
	}
}

// handleEC2Instance manages the EC2 instance
func handleEC2Instance(client *ec2.Client, desired ConfigResourcesEC2Instance) {
	instanceID, currentState, err := findInstanceByName(client, desired.Name)
	if err != nil {
		log.Fatalf("Failed to check for existing EC2 instance: %v", err)
	}

	if instanceID != "" {
		fmt.Printf("EC2 instance already exists with ID: %s\n", instanceID)
		fmt.Println("Current State:")
		fmt.Printf("  Instance Type: %s\n", *currentState.InstanceType)
		fmt.Printf("  AMI: %s\n", *currentState.ImageId)
		fmt.Printf("  Key Name: %s\n", *currentState.KeyName)
		fmt.Printf("  Security Groups: %v\n", currentState.SecurityGroups)
		fmt.Printf("  Tags: %v\n", currentState.Tags)

		changes := compareEC2States(currentState, desired)
		if len(changes) > 0 {
			fmt.Println("\nChanges to be applied:")
			for _, change := range changes {
				fmt.Println(change)
			}

			fmt.Print("\nDo you want to apply these changes? (yes/no): ")
			var approval string
			fmt.Scanln(&approval)

			if approval == "yes" {
				err = updateEC2Instance(client, instanceID, desired)
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
		instanceID, err = createEC2Instance(client, desired)
		if err != nil {
			log.Fatalf("Failed to create EC2 instance: %v", err)
		}
		fmt.Printf("Created EC2 instance with ID: %s\n", instanceID)
	}
}

// handleS3Bucket manages the S3 bucket
func handleS3Bucket(client *s3.Client, desired ConfigResourcesS3Bucket) {
	bucketName := desired.Name
	exists, err := checkS3BucketExists(client, bucketName)
	if err != nil {
		log.Fatalf("Failed to check for existing S3 bucket: %v", err)
	}

	if exists {
		fmt.Printf("S3 bucket already exists: %s\n", bucketName)
	} else {
		err = createS3Bucket(client, desired)
		if err != nil {
			log.Fatalf("Failed to create S3 bucket: %v", err)
		}
		fmt.Printf("Created S3 bucket: %s\n", bucketName)
	}
}

// handleRDSInstance manages the RDS instance
func handleRDSInstance(client *rds.Client, desired ConfigResourcesRDSInstance) {
	instanceID, currentState, err := findRDSInstanceByName(client, desired.Name)
	if err != nil {
		log.Fatalf("Failed to check for existing RDS instance: %v", err)
	}

	if instanceID != "" {
		fmt.Printf("RDS instance already exists with ID: %s\n", instanceID)
		fmt.Println("Current State:")
		fmt.Printf("  Engine: %s\n", *currentState.Engine)
		fmt.Printf("  Engine Version: %s\n", *currentState.EngineVersion)
		fmt.Printf("  Instance Class: %s\n", *currentState.DBInstanceClass)
		fmt.Printf("  Allocated Storage: %d\n", *currentState.AllocatedStorage)
		fmt.Printf("  Tags: %v\n", currentState.TagList)

		changes := compareRDSStates(currentState, desired)
		if len(changes) > 0 {
			fmt.Println("\nChanges to be applied:")
			for _, change := range changes {
				fmt.Println(change)
			}

			fmt.Print("\nDo you want to apply these changes? (yes/no): ")
			var approval string
			fmt.Scanln(&approval)

			if approval == "yes" {
				err = updateRDSInstance(client, instanceID, desired)
				if err != nil {
					log.Fatalf("Failed to update RDS instance: %v", err)
				}
				fmt.Println("Changes applied successfully.")
			} else {
				fmt.Println("Changes rejected.")
			}
		} else {
			fmt.Println("No changes required.")
		}
	} else {
		instanceID, err = createRDSInstance(client, desired)
		if err != nil {
			log.Fatalf("Failed to create RDS instance: %v", err)
		}
		fmt.Printf("Created RDS instance with ID: %s\n", instanceID)
	}
}

// Functions for EC2, S3, and RDS (create, update, compare, etc.) will be added here.