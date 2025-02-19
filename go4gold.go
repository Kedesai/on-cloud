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
}

// handleEC2Instance manages the EC2 instance
func handleEC2Instance(client *ec2.Client, desired ConfigResourcesEC2Instance) error {
	instanceID, currentState, err := findInstanceByName(client, desired.Name)
	if err != nil {
		return fmt.Errorf("failed to check for existing EC2 instance: %v", err)
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
					return fmt.Errorf("failed to update EC2 instance: %v", err)
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
			return fmt.Errorf("failed to create EC2 instance: %v", err)
		}
		fmt.Printf("Created EC2 instance with ID: %s\n", instanceID)
	}

	return nil
}

// handleS3Bucket manages the S3 bucket
func handleS3Bucket(client *s3.Client, desired ConfigResourcesS3Bucket) error {
	bucketName := desired.Name
	uniqueBucketName, err := getUniqueBucketName(client, bucketName)
	if err != nil {
		return fmt.Errorf("failed to get unique bucket name: %v", err)
	}

	if uniqueBucketName != bucketName {
		fmt.Printf("Bucket name '%s' already exists. Using '%s' instead.\n", bucketName, uniqueBucketName)
	}

	err = createS3Bucket(client, uniqueBucketName, desired.ACL, desired.Tags)
	if err != nil {
		return fmt.Errorf("failed to create S3 bucket: %v", err)
	}
	fmt.Printf("Created S3 bucket: %s\n", uniqueBucketName)

	return nil
}

// getUniqueBucketName finds a unique bucket name by appending a number if necessary
func getUniqueBucketName(client *s3.Client, baseName string) (string, error) {
	name := baseName
	for i := 0; i < 10; i++ { // Try up to 10 times
		exists, err := checkS3BucketExists(client, name)
		if err != nil {
			return "", fmt.Errorf("failed to check bucket existence: %v", err)
		}
		if !exists {
			return name, nil
		}
		name = fmt.Sprintf("%s-%d", baseName, i+1)
	}
	return "", fmt.Errorf("could not find a unique bucket name after 10 attempts")
}

// checkS3BucketExists checks if an S3 bucket exists
func checkS3BucketExists(client *s3.Client, bucketName string) (bool, error) {
	_, err := client.HeadBucket(context.TODO(), &s3.HeadBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check bucket existence: %v", err)
	}
	return true, nil
}

// createS3Bucket creates an S3 bucket with retry logic
func createS3Bucket(client *s3.Client, bucketName, acl string, tags map[string]string) error {
	err := retry.Do(
		func() error {
			_, err := client.CreateBucket(context.TODO(), &s3.CreateBucketInput{
				Bucket: aws.String(bucketName),
				ACL:    types.BucketCannedACL(acl),
			})
			if err != nil {
				return fmt.Errorf("failed to create bucket: %v", err)
			}

			// Add tags to the bucket
			if len(tags) > 0 {
				tagSet := make([]types.Tag, 0, len(tags))
				for key, value := range tags {
					tagSet = append(tagSet, types.Tag{
						Key:   aws.String(key),
						Value: aws.String(value),
					})
				}
				_, err = client.PutBucketTagging(context.TODO(), &s3.PutBucketTaggingInput{
					Bucket: aws.String(bucketName),
					Tagging: &types.Tagging{
						TagSet: tagSet,
					},
				})
				if err != nil {
					return fmt.Errorf("failed to add tags to bucket: %v", err)
				}
			}

			return nil
		},
		retry.Attempts(3),
		retry.Delay(2*time.Second),
		retry.OnRetry(func(n uint, err error) {
			log.Printf("Retry %d: %v", n, err)
		}),
	)
	return err
}

// handleRDSInstance manages the RDS instance
func handleRDSInstance(client *rds.Client, desired ConfigResourcesRDSInstance) error {
	instanceID, currentState, err := findRDSInstanceByName(client, desired.Name)
	if err != nil {
		return fmt.Errorf("failed to check for existing RDS instance: %v", err)
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
					return fmt.Errorf("failed to update RDS instance: %v", err)
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
			return fmt.Errorf("failed to create RDS instance: %v", err)
		}
		fmt.Printf("Created RDS instance with ID: %s\n", instanceID)
	}

	return nil
}