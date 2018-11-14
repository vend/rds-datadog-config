# RDS DataDog MySQL Agent Config

This tool will auto-generate Datadog Agent MySQL configs for AWS RDS/Aurora instances.

It works by:
  1. Determining the current Region + VPC (via EC2 metadata, or manually specified)
  2. Fetching all DB instances
  3. Fetching all DB clusters
  4. Filtering on VPC
  5. Filtering on instances with credentials
  6. Generating YAML to STDOUT
  7. Adds cluster + instance tags.

Passwords, and individial instance/cluster configs are stored in `passwords.ini`

Depending on the particular state of each DB, various Datadog options are turned on/off.

## Usage

```
./rds-datadog-config  > mysql.yaml
mv mysql.yaml /etc/dd-agent/conf.d/mysql.yaml
sudo service datadog-agent restart
````

In development, you'll probably be using aws-vault, and need to disable EC2 metadata collection:

```
aws-vault exec production-daily-admin -- ./rds-datadog-config -vpcid="vpc-XYZ" -noec2metadata > mysql_tool.yaml
```

## Required IAM EC2 Policy

```
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Sid": "VisualEditor0",
            "Effect": "Allow",
            "Action": [
                "rds:DescribeDBInstances",
                "rds:DescribeDBClusters"
            ],
            "Resource": "*"
        }
    ]
}
```

## Passwords.ini

This file contains the username + passwords for each DB cluster.

```
username = datadog <global mysql username>

[cluster-1]
password = XXXYYY <mysql password>

[cluster-2]
password = YYYZZZ <mysql password>
extra_performance = true
rename_cluster = my-payments-cluster <ability to set a friendly #database_group: tag>
rename_instance_DBINSTANCENAME = friendly-db-instance-name <ability to set a friendly #dbinstanceidentifier: tag>
```

## Example MySQL Grant

```
GRANT PROCESS ON *.* TO 'datadog_username'@'IP/MASK' IDENTIFIED BY 'xyz';
GRANT REFERENCES ON *.* to 'datadog_username'@'IP/MASK';
GRANT REPLICATION CLIENT TO 'datadog_username'@'IP/MASK'
```

## TODO

- Periodic script to auto-update
- Containerise
- allow the cluster + instance tag names to be changed
- work out when it's appropriate to auto-turn on `extra_performance_metrics`
- ability to remove our custom `processlist_size` option
- IAM role-based authentication
- Check security groups, to confirm access
- Everything is a config, which can make the code challanging to parse. Come up with better names.