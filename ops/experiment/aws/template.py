#!/usr/bin/env python3
"""Launch template data and IAM policy for the hosts started on demand.

  template.py SCRIPT AMI TYPE SUBNET SG RUN IOPS THROUGHPUT   launch template data
  template.py s3 BUCKET                                       bucket policy
  template.py policy REGION ACCOUNT RUN                       policy for host 0
"""
import base64, json, sys

if sys.argv[1] == "s3":
    bucket = sys.argv[2]
    print(json.dumps({"Version": "2012-10-17", "Statement": [
        {"Effect": "Allow", "Action": ["s3:GetObject", "s3:ListBucket"],
         "Resource": [f"arn:aws:s3:::{bucket}", f"arn:aws:s3:::{bucket}/*"]},
        # Nodes publish results and discovery, but cannot raise their own budget.
        {"Effect": "Allow", "Action": "s3:PutObject",
         "Resource": [f"arn:aws:s3:::{bucket}/results/*", f"arn:aws:s3:::{bucket}/agents",
                      f"arn:aws:s3:::{bucket}/agents.d/*"]},
    ]}))
elif sys.argv[1] == "policy":
    region, account, run = sys.argv[2:]
    print(json.dumps({"Version": "2012-10-17", "Statement": [
        {"Effect": "Allow", "Action": "ec2:DescribeInstances", "Resource": "*"},
        # Instances only through a launch template of this account, with
        # every parameter coming from it (no override of type, role or network).
        {"Effect": "Allow", "Action": "ec2:RunInstances", "Resource": "*",
         "Condition": {"ArnLike": {"ec2:LaunchTemplate": f"arn:aws:ec2:{region}:{account}:launch-template/*"},
                       "Bool": {"ec2:IsLaunchTemplateResource": "true"}}},
        {"Effect": "Allow", "Action": "ec2:RunInstances",
         "Resource": f"arn:aws:ec2:{region}:{account}:launch-template/*"},
        # The tags the template puts on what it creates.
        {"Effect": "Allow", "Action": "ec2:CreateTags", "Resource": f"arn:aws:ec2:{region}:{account}:*/*",
         "Condition": {"StringEquals": {"ec2:CreateAction": "RunInstances"}}},
        {"Effect": "Allow", "Action": "iam:PassRole", "Resource": f"arn:aws:iam::{account}:role/{run}"},
    ]}))
else:
    script, ami, kind, subnet, sg, run, iops, throughput = sys.argv[1:]
    tags = [{"Key": "Project", "Value": "indexus-experiment"}, {"Key": "Run", "Value": run}, {"Key": "Name", "Value": run + "-hx"}]
    print(json.dumps({
        "ImageId": ami, "InstanceType": kind, "UserData": base64.b64encode(open(script, "rb").read()).decode(),
        "IamInstanceProfile": {"Name": run}, "InstanceInitiatedShutdownBehavior": "terminate",
        "NetworkInterfaces": [{"DeviceIndex": 0, "SubnetId": subnet, "Groups": [sg], "AssociatePublicIpAddress": True}],
        "BlockDeviceMappings": [{"DeviceName": "/dev/xvda", "Ebs": {"VolumeSize": 40, "VolumeType": "gp3", "Iops": int(iops), "Throughput": int(throughput), "DeleteOnTermination": True}}],
        "TagSpecifications": [{"ResourceType": "instance", "Tags": tags}],
    }))
