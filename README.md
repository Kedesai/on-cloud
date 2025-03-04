# on-cloud
Deploy AWS resources using the familiar yaml to cloud. use the ec2-config-generator to generate ec2 config: https://github.com/Kedesai/ec2-config-generator. This tool generates the infra.yaml and variables.yaml. then use the oncloud cloud go4gold binary to run those yamlfiles. The implementation is idempotent meaning the resources if installed will not be installed again.
