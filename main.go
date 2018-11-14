package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/ec2metadata"
	"github.com/aws/aws-sdk-go-v2/aws/endpoints"
	"github.com/aws/aws-sdk-go-v2/aws/external"
	"github.com/aws/aws-sdk-go-v2/service/rds"

	"gopkg.in/ini.v1"
	"gopkg.in/yaml.v2"
)

// Global logger
var logger *log.Logger

// Keeping track of the useful parts of the Clusters endpoint
type ClusterMember struct {
	Cluster            string
	InstanceIdentifier string
	IsWriter           bool
}

// Keeping track of the useful part of the Instances endpoint
type DatabaseState struct {
	Engine            string
	Cluster           string
	Identifier        string
	Hostname          string
	Port              int64
	VPC               string
	Username          string
	Password          string
	MySQLReplication  bool
	AuroraReadReplica bool
}

// The ultimate goal here, the Datadog MySQL config
type DatadogConfig struct {
	Server         string   `yaml:"server"`
	User           string   `yaml:"user"`
	Pass           string   `yaml:"pass"`
	Port           int64    `yaml:"port"`
	ConnectTimeout int64    `yaml:"connect_timeout,omitempty"`
	Tags           []string `yaml:"tags,omitempty"`
	Options        struct {
		Replication      bool `yaml:"replication"`
		ExtraStatus      bool `yaml:"extra_status_metrics"`
		ExtraInnoDb      bool `yaml:"extra_innodb_metrics"`
		DisableInnoDb    bool `yaml:"disable_innodb_metrics"`
		ExtraPerformance bool `yaml:"extra_performance_metrics"`
		SchemaSize       bool `yaml:"schema_size_metrics"`
		ProcessList      bool `yaml:"processlist_size"`
	} `yaml:"options"`
}

// The placeholder YAML for the DD config
type DatadogOuter struct {
	InitConfig []string        `yaml:"init_config"`
	Instances  []DatadogConfig `yaml:"instances"`
}

func ParseAwsRdsInstance(instance rds.DBInstance) (DatabaseState, error) {
	// fmt.Println(instance)
	var config DatabaseState

	config.Hostname = *instance.Endpoint.Address
	config.Identifier = *instance.DBInstanceIdentifier
	config.VPC = *instance.DBSubnetGroup.VpcId
	config.Port = *instance.Endpoint.Port

	switch engine := *instance.Engine; engine {
	case "mysql":
		config.Engine = "mysql"

		// In traditional MySQL, it's be a master, or slave.
		if instance.ReadReplicaSourceDBInstanceIdentifier != nil {
			// If we're a slave, let's take the parent as the "clustering" name
			config.Cluster = *instance.ReadReplicaSourceDBInstanceIdentifier
			config.MySQLReplication = true
		} else {
			// If we're a master, the cluser is just the DB name
			config.Cluster = *instance.DBInstanceIdentifier
			config.MySQLReplication = false
		}

	case "aurora-mysql":
		config.Engine = "aurora"
		config.Cluster = *instance.DBClusterIdentifier
	case "aurora":
		config.Engine = "aurora"
		config.Cluster = *instance.DBClusterIdentifier
	default:
		return config, errors.New("Unknown RDS engine type")
	}

	return config, nil

}

func FindAuroraMember(members []ClusterMember, searchIdentifer string) (ClusterMember, error) {
	for _, member := range members {
		// fmt.Println(member.InstanceIdentifier, searchIdentifer)
		if strings.EqualFold(member.InstanceIdentifier, searchIdentifer) {
			return member, nil
		}
	}
	var empty ClusterMember
	return empty, errors.New("Aurora instance can't be found")
}

func ParseAwsClusterMembers(clusterResponse rds.DBCluster) ([]ClusterMember, error) {
	// fmt.Println(clusterResponse)
	var clusterMembers []ClusterMember
	for _, responseMember := range clusterResponse.DBClusterMembers {
		var member ClusterMember
		member.Cluster = *clusterResponse.DBClusterIdentifier
		member.InstanceIdentifier = *responseMember.DBInstanceIdentifier
		member.IsWriter = *responseMember.IsClusterWriter
		clusterMembers = append(clusterMembers, member)
	}
	return clusterMembers, nil
}

func ReadConfig(filename string) *ini.File {

	local_ini, err := ini.Load(filename)
	if err != nil {
		fmt.Printf("Fail to read file: %v", err)
		os.Exit(1)
	}

	fmt.Println("%v", local_ini)

	return local_ini

}

func ClusterVPCFilter(rdsConfigs []DatabaseState, vpc string) []DatabaseState {
	var filteredConfigs []DatabaseState
	for _, config := range rdsConfigs {
		if config.VPC == vpc {
			filteredConfigs = append(filteredConfigs, config)
		}
	}
	return filteredConfigs
}

func ClusterIniFilter(rdsConfigs []DatabaseState, iniConfig *ini.File) []DatabaseState {
	var filteredConfigs []DatabaseState

	for _, config := range rdsConfigs {
		var _, err = iniConfig.GetSection(config.Cluster)
		if err == nil {
			filteredConfigs = append(filteredConfigs, config)
		}
	}
	return filteredConfigs
}

func CreateDatadogConfigs(rdsConfigs []DatabaseState, clusterMembers []ClusterMember, iniConfig *ini.File) []DatadogConfig {
	var ddConfigs []DatadogConfig
	var default_username = iniConfig.Section("").Key("user").MustString("dummy_username")

	for _, rdsConfig := range rdsConfigs {
		var ddConfig DatadogConfig

		// Main config stuff
		ddConfig.Server = rdsConfig.Hostname
		ddConfig.Port = rdsConfig.Port
		ddConfig.ConnectTimeout = iniConfig.Section(rdsConfig.Cluster).Key("connect_timeout").MustInt64(1)
		ddConfig.User = iniConfig.Section(rdsConfig.Cluster).Key("user").MustString(default_username)
		ddConfig.Pass = iniConfig.Section(rdsConfig.Cluster).Key("password").MustString("dummy_password")

		// Clustering tag can be overridden
		var clusterTag = iniConfig.Section(rdsConfig.Cluster).Key("rename_cluster").MustString(rdsConfig.Cluster)
		var instanceTag = iniConfig.Section(rdsConfig.Cluster).Key("rename_instance_" + rdsConfig.Identifier).MustString(rdsConfig.Identifier)

		// Set DD Tags
		var tags []string
		tags = append(tags, "dbinstanceidentifier:"+instanceTag)
		tags = append(tags, "database_group:"+clusterTag)
		ddConfig.Tags = tags

		// Consistent stuff
		ddConfig.Options.SchemaSize = true
		ddConfig.Options.ProcessList = true
		ddConfig.Options.ExtraStatus = true

		// Configurable, as I'm not sure if it can be globally turned on, or automatic yet
		ddConfig.Options.ExtraPerformance = iniConfig.Section(rdsConfig.Cluster).Key("extra_performance").MustBool(false)

		switch engine := rdsConfig.Engine; engine {
		case "aurora":
			// Find what kind of member we are, and disable some stuff if necessary
			ddConfig.Options.Replication = false
			var clusterMember, err = FindAuroraMember(clusterMembers, rdsConfig.Identifier)
			if err == nil {
				ddConfig.Options.DisableInnoDb = !clusterMember.IsWriter
				ddConfig.Options.ExtraInnoDb = clusterMember.IsWriter
			} else {
				panic("Can't find Aurora cluster member: " + rdsConfig.Identifier)
			}
		case "mysql":
			// Always enable
			ddConfig.Options.DisableInnoDb = false
			ddConfig.Options.ExtraInnoDb = true
			// Depends if we're a slave
			ddConfig.Options.Replication = rdsConfig.MySQLReplication
		default:
			logger.Println("Unknown instnace engine type")
			os.Exit(22)
		}

		ddConfigs = append(ddConfigs, ddConfig)
	}
	return ddConfigs
}

func main() {

	var databaseStates []DatabaseState
	var clusterMembers []ClusterMember

	// Command-line flags
	var password_filename = flag.String("passwords", "passwords.ini", "INI file where passwords are stored")
	var noEC2Metadata = flag.Bool("noec2metadata", false, "Disable EC2 Metadata for detecting Region+VPC (useful for dev)")
	var vpcInterface = flag.String("ec2interface", "eth0", "Interface for detecting VPC")
	var defaultRegion = flag.String("region", endpoints.UsWest2RegionID, "Default region (when not using EC2 metadata)")
	var defaultVPC = flag.String("vpcid", "dummy", "Default VPC_ID for RDS discovery (when not using EC2 metadata)")

	flag.Parse()

	// Items we must fill
	var filtered_vpc string
	var aws_region string

	logger := log.New(os.Stderr, "", log.Lshortfile)

	// Read local ini file
	local_ini, err := ini.Load(*password_filename)
	if err != nil {
		logger.Printf("Can't open INI file: %s", *password_filename)
		os.Exit(10)
	}

	// Using the SDK's default configuration, loading additional config
	// and credentials values from the environment variables, shared
	// credentials, and shared configuration files
	cfg, err := external.LoadDefaultAWSConfig()
	if err != nil {
		logger.Printf("unable to load SDK config: %s ", err.Error())
		os.Exit(11)
	}

	// Adjust the default timeouts. Mainly to make EC2Metadata give up if necessary
	var awsTimeout = time.Duration(2 * time.Second)
	awsHttpClient := http.Client{
		Timeout: awsTimeout,
	}

	cfg.HTTPClient = &awsHttpClient

	// Shall we autodetect some settings?
	if !*noEC2Metadata {
		// EC2 Metadata, which we'll use to detect the VPC + Region + (eventually) security groups
		mdService := ec2metadata.New(cfg)
		if mdService.Available() {
			detectedRegion, err := mdService.Region()
			if err != nil {
				logger.Println("Can't detect EC2Metadata region.")
				os.Exit(13)

			} else {
				logger.Printf("Detected EC2Metadata region: %s", detectedRegion)
				aws_region = detectedRegion

				// Grab MAC address
				var iface, err = net.InterfaceByName(*vpcInterface)
				if err != nil {
					logger.Printf("Can't detect MAC address for %s", *vpcInterface)
					os.Exit(14)
				} else {
					logger.Printf("%s MAC %s", *vpcInterface, iface.HardwareAddr)
				}

				// Grab VPC
				var vpcURL = fmt.Sprintf("network/interfaces/macs/%s/vpc-id", iface.HardwareAddr)
				logger.Printf("URL: %s", vpcURL)
				filtered_vpc, err = mdService.GetMetadata(vpcURL)
				if err != nil {
					logger.Printf("Can't discover VPC: %s", err.Error())
					os.Exit(15)
				} else {
					logger.Printf("Detected VPC: %s", filtered_vpc)
				}

			}
		} else {
			logger.Println("Can't talk to EC2 metadata endpoint. See the help to specify VPC+Region manually.")
			os.Exit(16)
		}
	} else {
		// We must be manually specifiying
		aws_region = *defaultRegion
		filtered_vpc = *defaultVPC
	}

	logger.Printf("Region: %s, VPC-ID: %s", aws_region, filtered_vpc)

	// Set the AWS Region that the service clients should use
	cfg.Region = aws_region

	// Using the Config value, create the DynamoDB client
	rdsService := rds.New(cfg)

	// Build the request with its input parameters
	requestInstances := rdsService.DescribeDBInstancesRequest(&rds.DescribeDBInstancesInput{})

	// Send the request, and get the response or error back
	resp, err := requestInstances.Send()
	if err != nil {
		panic("failed to describe rds instances, " + err.Error())
	}

	requestClusters := rdsService.DescribeDBClustersRequest(&rds.DescribeDBClustersInput{})
	responseClusters, err := requestClusters.Send()
	if err != nil {
		panic("failed to describe clusters, " + err.Error())
	}
	// fmt.Println("Clusters:")
	for _, responseCluster := range responseClusters.DBClusters {
		var parsedMembers, err = ParseAwsClusterMembers(responseCluster)
		if err != nil {
			fmt.Errorf("Can't parse Cluster members: %s", err.Error())
			os.Exit(10)
		} else {
			clusterMembers = append(clusterMembers, parsedMembers...)
		}
	}

	// logger.Println("DEBUG")
	// logger.Println(clusterMembers)

	// Collate the Configs
	for _, instanceResponse := range resp.DBInstances {
		var instanceState, err = ParseAwsRdsInstance(instanceResponse)
		if err != nil {
			fmt.Errorf("Cant parse RDS info: %s", err.Error())
			os.Exit(11)
		} else {
			databaseStates = append(databaseStates, instanceState)
		}
	}
	// Sort configs by cluster, easier for humans
	sort.Slice(databaseStates, func(i, j int) bool {
		if databaseStates[i].Cluster == databaseStates[j].Cluster {
			return databaseStates[i].Identifier < databaseStates[j].Identifier
		} else {
			return databaseStates[i].Cluster < databaseStates[j].Cluster
		}
	})

	logger.Println("Original Instances")
	for _, config := range databaseStates {
		logger.Printf("%s %s ", config.Cluster, config.Identifier)
	}

	// fmt.Println("Filtered Configs")
	databaseStates = ClusterVPCFilter(databaseStates, filtered_vpc)
	logger.Println("VPC Filtered Instances")
	for _, config := range databaseStates {
		logger.Printf("%s %s ", config.Cluster, config.Identifier)
	}

	databaseStates = ClusterIniFilter(databaseStates, local_ini)
	logger.Println("Credentials from INI")
	for _, config := range databaseStates {
		logger.Printf("%s %s ", config.Cluster, config.Identifier)
	}

	var ddOuter DatadogOuter
	ddOuter.Instances = CreateDatadogConfigs(databaseStates, clusterMembers, local_ini)
	ddFlat, err := yaml.Marshal(&ddOuter)

	// Use STDOUT, so we can pipe the output to a file
	fmt.Printf("%s", ddFlat)
}
